package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/adapter"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	WorkloadFinalizer = "accelerator.qcrs.io/workload-protection"
	LeaseFinalizer    = "accelerator.qcrs.io/release-protection"

	ConditionAccepted     = "Accepted"
	ConditionSelected     = "Selected"
	ConditionReserved     = "Reserved"
	ConditionRuntimeReady = "RuntimeReady"
	ConditionPodReady     = "PodReady"
	ConditionDegraded     = "Degraded"

	decisionPolicyAnnotation = "accelerator.qcrs.io/decision-policy"
	decisionReasonAnnotation = "accelerator.qcrs.io/decision-reason"
	workloadNameAnnotation   = "accelerator.qcrs.io/workload-name"
	workloadNameLabel        = "accelerator.qcrs.io/workload"
	workloadUIDLabel         = "accelerator.qcrs.io/workload-uid"

	defaultReservationTTL        = 5 * time.Minute
	unschedulableRetry           = 15 * time.Second
	defaultEndpointCheckInterval = 15 * time.Second
)

type WorkloadReconciler struct {
	client.Client
	// Lifecycle state must be read after our previous writes are committed;
	// inventory discovery may continue to use the eventually consistent cache.
	APIReader client.Reader
	Scheme    *runtime.Scheme

	Inventory contracts.InventoryReader
	Policy    contracts.PolicyEngine
	Adapters  *adapter.Registry
	Injector  contracts.PodInjector
	Recorder  record.EventRecorder
	Now       func() time.Time
}

func (r *WorkloadReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&acceleratorv1alpha1.AcceleratorWorkload{}).
		Owns(&acceleratorv1alpha1.AcceleratorLease{}).
		Owns(&corev1.Pod{}).
		WithOptions(controlleroptions.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *WorkloadReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	workload := &acceleratorv1alpha1.AcceleratorWorkload{}
	if err := r.stateReader().Get(ctx, request.NamespacedName, workload); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workload.DeletionTimestamp.IsZero() {
		return r.finalizeWorkload(ctx, workload)
	}
	if err := r.validateDependencies(); err != nil {
		return ctrl.Result{}, err
	}

	managedResources, err := r.managedResources(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := validateWorkload(workload, managedResources); err != nil {
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionAccepted, metav1.ConditionFalse, "Invalid", err.Error(), r.now())
		return ctrl.Result{}, r.updateWorkloadStatus(ctx, workload)
	}
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionAccepted, metav1.ConditionTrue, "Valid", "workload specification is valid", r.now())

	if !controllerutil.ContainsFinalizer(workload, WorkloadFinalizer) {
		controllerutil.AddFinalizer(workload, WorkloadFinalizer)
		if err := r.Update(ctx, workload); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if workload.Status.AllocationRef == "" {
		if result, waiting := r.waitForFallback(workload); waiting {
			return result, nil
		}
		return r.selectAndCreateLease(ctx, workload)
	}

	lease := &acceleratorv1alpha1.AcceleratorLease{}
	leaseKey := types.NamespacedName{Namespace: workload.Namespace, Name: workload.Status.AllocationRef}
	if err := r.stateReader().Get(ctx, leaseKey, lease); err != nil {
		if apierrors.IsNotFound(err) {
			if workloadPhaseTerminal(workload.Status.Phase) {
				return ctrl.Result{}, nil
			}
			workload.Status.AllocationRef = ""
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
			return ctrl.Result{Requeue: true}, r.updateWorkloadStatus(ctx, workload)
		}
		return ctrl.Result{}, err
	}
	if !lease.DeletionTimestamp.IsZero() {
		return r.finalizeDeletingLease(ctx, workload, lease)
	}

	if allocation.IsExpired(lease, r.now()) {
		return r.expireLease(ctx, workload, lease)
	}
	if failureForAttempt(workload, workload.Status.AllocationAttempt) != nil &&
		lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseReleased {
		return r.continueStartupFailure(ctx, workload, lease)
	}

	switch lease.Status.Phase {
	case "":
		if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhasePending); err != nil {
			return ctrl.Result{}, err
		}
		if lease.Spec.TTL.Duration <= 0 {
			return ctrl.Result{}, contracts.NewError(contracts.ErrorKindInvalid, "initialize allocation lease", "lease TTL must be positive")
		}
		createdAt := lease.CreationTimestamp.Time
		if createdAt.IsZero() {
			createdAt = r.now()
		}
		expiresAt := metav1.NewTime(createdAt.Add(lease.Spec.TTL.Duration))
		lease.Status.ExpiresAt = &expiresAt
		return ctrl.Result{Requeue: true}, r.updateLeaseStatus(ctx, lease)
	case acceleratorv1alpha1.AllocationPhasePending:
		return r.reserveLease(ctx, workload, lease)
	case acceleratorv1alpha1.AllocationPhaseReserved:
		return r.ensurePod(ctx, workload, lease)
	case acceleratorv1alpha1.AllocationPhaseBound:
		return r.syncBoundPod(ctx, workload, lease)
	case acceleratorv1alpha1.AllocationPhaseReleasing:
		if err := r.releaseLease(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	case acceleratorv1alpha1.AllocationPhaseReleased:
		return r.syncTerminalStatus(ctx, workload, lease)
	case acceleratorv1alpha1.AllocationPhaseFailed, acceleratorv1alpha1.AllocationPhaseExpired:
		workload.Status.AllocationRef = ""
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
		return ctrl.Result{RequeueAfter: time.Second}, r.updateWorkloadStatus(ctx, workload)
	default:
		return ctrl.Result{}, fmt.Errorf("unknown allocation phase %q", lease.Status.Phase)
	}
}

func (r *WorkloadReconciler) resolveProvider(
	ctx context.Context,
	providerName,
	poolName string,
) (*acceleratorv1alpha1.AcceleratorProvider, *acceleratorv1alpha1.AcceleratorPool, contracts.Provider, error) {
	providerObject := &acceleratorv1alpha1.AcceleratorProvider{}
	if err := r.stateReader().Get(ctx, types.NamespacedName{Name: providerName}, providerObject); err != nil {
		return nil, nil, nil, err
	}
	pool := &acceleratorv1alpha1.AcceleratorPool{}
	if err := r.stateReader().Get(ctx, types.NamespacedName{Name: poolName}, pool); err != nil {
		return nil, nil, nil, err
	}
	if pool.Spec.ProviderRef.Name != providerName {
		return nil, nil, nil, fmt.Errorf("pool %q references provider %q instead of %q", poolName, pool.Spec.ProviderRef.Name, providerName)
	}
	execution, err := r.Adapters.Get(providerObject.Spec.Driver)
	if err != nil {
		return nil, nil, nil, err
	}
	provider := execution.Provider()
	if provider == nil {
		return nil, nil, nil, contracts.NewError(contracts.ErrorKindInvalid, "resolve execution adapter", fmt.Sprintf("driver %q returned no provider", providerObject.Spec.Driver))
	}
	return providerObject, pool, provider, nil
}

// resolveExecution resolves the Kubernetes configuration and then selects the
// registered execution adapter for the provider driver. Keeping this lookup in
// one place prevents the reservation path and the runtime-materialization path
// from accidentally using different implementations.
func (r *WorkloadReconciler) resolveExecution(
	ctx context.Context,
	providerName,
	poolName string,
) (*acceleratorv1alpha1.AcceleratorProvider, *acceleratorv1alpha1.AcceleratorPool, contracts.Provider, contracts.Allocator, error) {
	providerObject, pool, provider, err := r.resolveProvider(ctx, providerName, poolName)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	execution, err := r.Adapters.Get(providerObject.Spec.Driver)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	runtimeProvider := execution.Provider()
	allocator := execution.Allocator()
	if runtimeProvider == nil || allocator == nil {
		return nil, nil, nil, nil, contracts.NewError(contracts.ErrorKindInvalid, "resolve execution adapter", fmt.Sprintf("driver %q returned an incomplete adapter", providerObject.Spec.Driver))
	}
	if runtimeProvider.Driver() != provider.Driver() {
		return nil, nil, nil, nil, contracts.NewError(contracts.ErrorKindConflict, "resolve execution adapter", fmt.Sprintf("adapter driver %q does not match provider driver %q", runtimeProvider.Driver(), provider.Driver()))
	}
	return providerObject, pool, runtimeProvider, allocator, nil
}

func (r *WorkloadReconciler) allocatorForActiveLease(ctx context.Context, lease *acceleratorv1alpha1.AcceleratorLease) (contracts.Allocator, error) {
	if lease == nil {
		return nil, contracts.NewError(contracts.ErrorKindInvalid, "resolve execution allocator", "allocation Lease is required")
	}
	providerObject, _, _, allocator, err := r.resolveExecution(ctx, lease.Spec.ProviderRef.Name, lease.Spec.PoolRef.Name)
	if err != nil {
		return nil, err
	}
	if lease.Spec.Driver != "" && providerObject.Spec.Driver != lease.Spec.Driver {
		return nil, contracts.NewError(
			contracts.ErrorKindInvalid,
			"resolve execution allocator",
			fmt.Sprintf("provider driver %q no longer matches Lease driver %q", providerObject.Spec.Driver, lease.Spec.Driver),
		)
	}
	return allocator, nil
}

func (r *WorkloadReconciler) allocatorForRelease(ctx context.Context, lease *acceleratorv1alpha1.AcceleratorLease) (contracts.Allocator, error) {
	if lease == nil {
		return nil, contracts.NewError(contracts.ErrorKindInvalid, "resolve release allocator", "allocation Lease is required")
	}
	if lease.Spec.Driver != "" {
		execution, err := r.Adapters.Get(lease.Spec.Driver)
		if err != nil {
			return nil, err
		}
		allocator := execution.Allocator()
		if allocator == nil {
			return nil, contracts.NewError(contracts.ErrorKindInvalid, "resolve release allocator", fmt.Sprintf("driver %q returned no allocator", lease.Spec.Driver))
		}
		return allocator, nil
	}
	// Backward compatibility for Leases created before spec.driver existed.
	// New Leases never take this path.
	_, _, _, allocator, err := r.resolveExecution(ctx, lease.Spec.ProviderRef.Name, lease.Spec.PoolRef.Name)
	return allocator, err
}

func (r *WorkloadReconciler) managedResources(ctx context.Context) (map[corev1.ResourceName]struct{}, error) {
	pools := &acceleratorv1alpha1.AcceleratorPoolList{}
	if err := r.List(ctx, pools); err != nil {
		return nil, err
	}
	resources := map[corev1.ResourceName]struct{}{}
	for index := range pools.Items {
		pool := &pools.Items[index]
		if pool.Spec.Local != nil && pool.Spec.Local.ResourceName != "" {
			resources[pool.Spec.Local.ResourceName] = struct{}{}
		}
	}
	return resources, nil
}

func (r *WorkloadReconciler) updateWorkloadStatus(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload) error {
	workload.Status.ObservedGeneration = workload.Generation
	current := &acceleratorv1alpha1.AcceleratorWorkload{}
	if err := r.stateReader().Get(ctx, client.ObjectKeyFromObject(workload), current); err != nil {
		return err
	}
	if workload.ResourceVersion != "" && workload.ResourceVersion != current.ResourceVersion {
		return apierrors.NewConflict(acceleratorv1alpha1.GroupVersion.WithResource("acceleratorworkloads").GroupResource(), workload.Name, fmt.Errorf("lifecycle snapshot is stale"))
	}
	if reflect.DeepEqual(current.Status, workload.Status) {
		workload.ResourceVersion = current.ResourceVersion
		return nil
	}
	before := current.DeepCopy()
	current.Status = workload.Status
	if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	workload.ResourceVersion = current.ResourceVersion
	return nil
}

func (r *WorkloadReconciler) updateLeaseStatus(ctx context.Context, lease *acceleratorv1alpha1.AcceleratorLease) error {
	lease.Status.ObservedGeneration = lease.Generation
	current := &acceleratorv1alpha1.AcceleratorLease{}
	if err := r.stateReader().Get(ctx, client.ObjectKeyFromObject(lease), current); err != nil {
		return err
	}
	if lease.ResourceVersion != "" && lease.ResourceVersion != current.ResourceVersion {
		return apierrors.NewConflict(acceleratorv1alpha1.GroupVersion.WithResource("acceleratorleases").GroupResource(), lease.Name, fmt.Errorf("lifecycle snapshot is stale"))
	}
	if reflect.DeepEqual(current.Status, lease.Status) {
		lease.ResourceVersion = current.ResourceVersion
		return nil
	}
	before := current.DeepCopy()
	current.Status = lease.Status
	if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	lease.ResourceVersion = current.ResourceVersion
	return nil
}

func (r *WorkloadReconciler) removeLeaseFinalizer(ctx context.Context, lease *acceleratorv1alpha1.AcceleratorLease) error {
	if !controllerutil.ContainsFinalizer(lease, LeaseFinalizer) {
		return nil
	}
	controllerutil.RemoveFinalizer(lease, LeaseFinalizer)
	return r.Update(ctx, lease)
}

func (r *WorkloadReconciler) validateDependencies() error {
	if r.Client == nil || r.Scheme == nil || r.Inventory == nil || r.Policy == nil ||
		r.Adapters == nil || r.Injector == nil {
		return fmt.Errorf("workload reconciler dependencies are incomplete")
	}
	return nil
}

func (r *WorkloadReconciler) stateReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *WorkloadReconciler) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func setCondition(conditions *[]metav1.Condition, generation int64, conditionType string, status metav1.ConditionStatus, reason, message string, now time.Time) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(now),
	})
}

func reservationRequest(workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) contracts.ReservationRequest {
	request := reservationRequestFromLease(lease)
	request.WorkloadUID = string(workload.UID)
	return request
}

func reservationRequestFromLease(lease *acceleratorv1alpha1.AcceleratorLease) contracts.ReservationRequest {
	leaseKey := string(lease.UID)
	if leaseKey == "" {
		// controller-runtime's fake client does not assign API-server UIDs. The
		// production path always uses UID; namespace/name keeps unit-level
		// idempotence deterministic without weakening the live fencing token.
		leaseKey = lease.Namespace + "/" + lease.Name
	}
	return contracts.ReservationRequest{
		LeaseKey:       leaseKey,
		LeaseNamespace: lease.Namespace,
		LeaseName:      lease.Name,
		WorkloadUID:    string(lease.Spec.Owner.UID),
		ProviderName:   lease.Spec.ProviderRef.Name,
		PoolName:       lease.Spec.PoolRef.Name,
		Mode:           lease.Spec.Mode,
		Request:        lease.Spec.Request,
		Targets:        lease.Spec.Targets,
		TTL:            lease.Spec.TTL.Duration,
	}
}

func workloadPhaseTerminal(phase acceleratorv1alpha1.WorkloadPhase) bool {
	return phase == acceleratorv1alpha1.WorkloadPhaseSucceeded || phase == acceleratorv1alpha1.WorkloadPhaseFailed
}

func workloadIdentityLabels(workload *acceleratorv1alpha1.AcceleratorWorkload) map[string]string {
	identity := string(workload.UID)
	if identity == "" {
		identity = identitySuffix(workload.Name, "")
	}
	result := map[string]string{workloadUIDLabel: identity}
	if len(utilvalidation.IsValidLabelValue(workload.Name)) == 0 {
		result[workloadNameLabel] = workload.Name
	}
	return result
}

func copyMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func copyBoundEndpoints(source []acceleratorv1alpha1.BoundEndpoint) []acceleratorv1alpha1.BoundEndpoint {
	if source == nil {
		return nil
	}
	result := make([]acceleratorv1alpha1.BoundEndpoint, len(source))
	for index := range source {
		result[index] = source[index]
		result[index].Parameters = copyMap(source[index].Parameters)
	}
	return result
}

func copyAllocationTargets(source []acceleratorv1alpha1.AllocationTarget) []acceleratorv1alpha1.AllocationTarget {
	if source == nil {
		return nil
	}
	result := make([]acceleratorv1alpha1.AllocationTarget, len(source))
	for index := range source {
		result[index] = source[index]
		result[index].DeviceIDs = append([]string(nil), source[index].DeviceIDs...)
	}
	return result
}
