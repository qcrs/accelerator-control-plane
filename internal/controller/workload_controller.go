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

func (r *WorkloadReconciler) selectAndCreateLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload) (ctrl.Result, error) {
	attempt := workload.Status.AllocationAttempt + 1
	existing := &acceleratorv1alpha1.AcceleratorLease{}
	existingKey := types.NamespacedName{Namespace: workload.Namespace, Name: leaseName(workload.Name, string(workload.UID), attempt)}
	if err := r.stateReader().Get(ctx, existingKey, existing); err == nil {
		return r.restoreLeaseSelection(ctx, workload, existing, attempt)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	selectionRequest := contracts.SelectionRequest{
		Accelerator: workload.Spec.Accelerator,
		Placement:   workload.Spec.Placement,
		PodTemplate: workload.Spec.PodTemplate,
	}
	selectionRequest.PreferredMode, selectionRequest.Exclusions = fallbackSelectionContext(workload)
	candidates, err := r.Inventory.ListCandidates(ctx, selectionRequest)
	if err != nil {
		return ctrl.Result{}, err
	}
	decision, err := r.Policy.Select(ctx, selectionRequest, candidates)
	if err != nil {
		if contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable) {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionSelected, metav1.ConditionFalse, "Unschedulable", err.Error(), r.now())
			return ctrl.Result{RequeueAfter: unschedulableRetry}, r.updateWorkloadStatus(ctx, workload)
		}
		return ctrl.Result{}, err
	}

	providerObject, pool, provider, _, err := r.resolveExecution(ctx, decision.ProviderName, decision.PoolName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pool.Spec.Location != decision.Mode {
		return ctrl.Result{}, contracts.NewError(contracts.ErrorKindConflict, "select execution adapter", fmt.Sprintf("pool %q location %q does not match decision mode %q", pool.Name, pool.Spec.Location, decision.Mode))
	}
	if err := provider.Validate(providerObject, pool); err != nil {
		if isPermanentConfigurationError(err) {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionSelected, metav1.ConditionFalse, "InvalidProvider", err.Error(), r.now())
			return ctrl.Result{RequeueAfter: unschedulableRetry}, r.updateWorkloadStatus(ctx, workload)
		}
		return ctrl.Result{}, err
	}

	ttl := workload.Spec.Placement.ReservationTTL.Duration
	if ttl == 0 {
		ttl = defaultReservationTTL
	}
	lease := &acceleratorv1alpha1.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       leaseName(workload.Name, string(workload.UID), attempt),
			Namespace:  workload.Namespace,
			Finalizers: []string{LeaseFinalizer},
			Annotations: map[string]string{
				decisionPolicyAnnotation: decision.Policy,
				decisionReasonAnnotation: decision.Reason,
				workloadNameAnnotation:   workload.Name,
			},
			Labels: workloadIdentityLabels(workload),
		},
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			Owner: corev1.ObjectReference{
				APIVersion: acceleratorv1alpha1.GroupVersion.String(),
				Kind:       "AcceleratorWorkload",
				Namespace:  workload.Namespace,
				Name:       workload.Name,
				UID:        workload.UID,
			},
			WorkloadGeneration: workload.Generation,
			Request:            workload.Spec.Accelerator,
			Driver:             providerObject.Spec.Driver,
			ProviderRef:        acceleratorv1alpha1.ClusterObjectReference{Name: decision.ProviderName},
			PoolRef:            acceleratorv1alpha1.ClusterObjectReference{Name: decision.PoolName},
			Mode:               decision.Mode,
			Targets:            decision.Targets,
			TTL:                metav1.Duration{Duration: ttl},
		},
	}
	if err := controllerutil.SetControllerReference(workload, lease, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, lease); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		existing = &acceleratorv1alpha1.AcceleratorLease{}
		if err := r.stateReader().Get(ctx, client.ObjectKeyFromObject(lease), existing); err != nil {
			return ctrl.Result{}, err
		}
		return r.restoreLeaseSelection(ctx, workload, existing, attempt)
	}
	return r.recordLeaseSelection(ctx, workload, lease, attempt, decision.Policy, decision.Reason, r.now())
}

func (r *WorkloadReconciler) restoreLeaseSelection(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
	attempt int32,
) (ctrl.Result, error) {
	if lease.Spec.Owner.UID != workload.UID || !metav1.IsControlledBy(lease, workload) {
		return ctrl.Result{}, fmt.Errorf("lease %s/%s is owned by a different workload", lease.Namespace, lease.Name)
	}
	if lease.Spec.WorkloadGeneration != workload.Generation {
		if err := r.releaseLease(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
		workload.Status.SelectedMode = ""
		workload.Status.AllocationAttempt = attempt
		workload.Status.Decision = nil
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionSelected, metav1.ConditionFalse, "WorkloadChanged", "discarded an allocation selected for an older workload generation", r.now())
		return ctrl.Result{Requeue: true}, r.updateWorkloadStatus(ctx, workload)
	}

	policy := lease.Annotations[decisionPolicyAnnotation]
	reason := lease.Annotations[decisionReasonAnnotation]
	if reason == "" {
		reason = fmt.Sprintf("restored existing allocation lease %s after a status retry", lease.Name)
	}
	decidedAt := lease.CreationTimestamp.Time
	if decidedAt.IsZero() {
		decidedAt = r.now()
	}
	return r.recordLeaseSelection(ctx, workload, lease, attempt, policy, reason, decidedAt)
}

func (r *WorkloadReconciler) recordLeaseSelection(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
	attempt int32,
	policy, reason string,
	decidedAt time.Time,
) (ctrl.Result, error) {
	workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseReserving
	workload.Status.SelectedMode = lease.Spec.Mode
	workload.Status.AllocationRef = lease.Name
	workload.Status.AllocationAttempt = attempt
	workload.Status.Decision = &acceleratorv1alpha1.DecisionStatus{
		Mode:        lease.Spec.Mode,
		ProviderRef: lease.Spec.ProviderRef,
		PoolRef:     lease.Spec.PoolRef,
		Targets:     copyAllocationTargets(lease.Spec.Targets),
		Policy:      policy,
		Reason:      reason,
		DecidedAt:   metav1.NewTime(decidedAt),
	}
	workload.Status.Fallback.RequeueAt = nil
	workload.Status.Fallback.StartupStartedAt = nil
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionSelected, metav1.ConditionTrue, "Selected", reason, r.now())
	if err := r.updateWorkloadStatus(ctx, workload); err != nil {
		return ctrl.Result{}, err
	}
	eventReason := "Selected"
	if workload.Status.Fallback.FailedAttempts > 0 {
		eventReason = "FallbackSelected"
	}
	r.eventNormal(workload, eventReason, "attempt %d selected %s mode using lease %s: %s", attempt, lease.Spec.Mode, lease.Name, reason)
	return ctrl.Result{Requeue: true}, nil
}

func (r *WorkloadReconciler) reserveLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	request := reservationRequest(workload, lease)
	allocator, err := r.allocatorForActiveLease(ctx, lease)
	if err != nil {
		if isPermanentConfigurationError(err) || apierrors.IsNotFound(err) {
			return r.failReservationConfiguration(ctx, workload, lease, "ReservationConfigurationUnavailable", err)
		}
		return ctrl.Result{}, err
	}
	reservation, err := allocator.Reserve(ctx, request)
	if err != nil {
		if contracts.IsErrorKind(err, contracts.ErrorKindTransient) {
			// Acquisition/rollback may have an unknown commit result. Keep the
			// same Pending Lease and finalizer; do not spend a fallback attempt
			// or publish a terminal phase while ownership is unconfirmed.
			setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionFalse, "ReservationRetrying", err.Error(), r.now())
			if statusErr := r.updateLeaseStatus(ctx, lease); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionFalse, "ReservationRetrying", err.Error(), r.now())
			if statusErr := r.updateWorkloadStatus(ctx, workload); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
		reason := ""
		switch {
		case contracts.IsErrorKind(err, contracts.ErrorKindConflict), contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable):
			reason = "CapacityChanged"
		case contracts.IsErrorKind(err, contracts.ErrorKindInvalid), contracts.IsErrorKind(err, contracts.ErrorKindUnsupported):
			reason = "ReservationRejected"
		}
		if reason != "" {
			fallbackEnabled, _, _, _ := fallbackSettings(workload)
			if fallbackEnabled && !podWasReady(workload) &&
				(contracts.IsErrorKind(err, contracts.ErrorKindConflict) || contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable)) {
				setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionFalse, reason, err.Error(), r.now())
				if updateErr := r.updateLeaseStatus(ctx, lease); updateErr != nil {
					return ctrl.Result{}, updateErr
				}
				setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionFalse, reason, err.Error(), r.now())
				return r.handlePrePodStartupFailure(ctx, workload, lease, startupFailure{Reason: reason, Message: err.Error()})
			}
			if transitionErr := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseFailed); transitionErr != nil {
				return ctrl.Result{}, transitionErr
			}
			setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionFalse, reason, err.Error(), r.now())
			if updateErr := r.updateLeaseStatus(ctx, lease); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			if finalizerErr := r.removeLeaseFinalizer(ctx, lease); finalizerErr != nil {
				return ctrl.Result{}, finalizerErr
			}
			workload.Status.AllocationRef = ""
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionFalse, reason, err.Error(), r.now())
			return ctrl.Result{RequeueAfter: time.Second}, r.updateWorkloadStatus(ctx, workload)
		}
		return ctrl.Result{}, err
	}
	if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseReserved); err != nil {
		return ctrl.Result{}, err
	}
	expiration := reservation.ExpiresAt
	if lease.Status.ExpiresAt != nil && (expiration.IsZero() || lease.Status.ExpiresAt.Time.Before(expiration)) {
		expiration = lease.Status.ExpiresAt.Time
	}
	expiresAt := metav1.NewTime(expiration)
	lease.Status.Binding = acceleratorv1alpha1.RuntimeBinding{
		ID:        reservation.BindingID,
		Endpoints: copyBoundEndpoints(reservation.Endpoints),
	}
	lease.Status.ExpiresAt = &expiresAt
	setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionTrue, "Reserved", "accelerator capacity is reserved", r.now())
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionTrue, "Reserved", "accelerator capacity is reserved", r.now())
	if err := r.updateLeaseStatus(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.updateWorkloadStatus(ctx, workload); err != nil {
		return ctrl.Result{}, err
	}
	r.eventNormal(workload, "Reserved", "lease %s reserved %d %s device(s) in pool %s", lease.Name, lease.Spec.Request.Count, lease.Spec.Request.DeviceType, lease.Spec.PoolRef.Name)
	return ctrl.Result{Requeue: true}, nil
}

func (r *WorkloadReconciler) failReservationConfiguration(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
	reason string,
	cause error,
) (ctrl.Result, error) {
	// An earlier Reserve may have acquired a Claim whose rollback failed.
	// Losing live configuration does not prove that ownership was never taken.
	// Resolve cleanup from the persisted driver and confirm it before making
	// the Lease terminal or removing its finalizer.
	allocator, err := r.allocatorForRelease(ctx, lease)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := allocator.Release(ctx, reservationRequestFromLease(lease)); err != nil {
		return ctrl.Result{}, err
	}
	if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseFailed); err != nil {
		return ctrl.Result{}, err
	}
	setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionFalse, reason, cause.Error(), r.now())
	if err := r.updateLeaseStatus(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.removeLeaseFinalizer(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	workload.Status.AllocationRef = ""
	workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionFalse, reason, cause.Error(), r.now())
	return ctrl.Result{RequeueAfter: time.Second}, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) ensurePod(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	providerObject, pool, provider, allocator, err := r.resolveExecution(ctx, lease.Spec.ProviderRef.Name, lease.Spec.PoolRef.Name)
	if err != nil {
		if isPermanentConfigurationError(err) || apierrors.IsNotFound(err) {
			return r.failRuntimePreparation(ctx, workload, lease, "ExecutionConfigurationUnavailable", err)
		}
		return ctrl.Result{}, err
	}
	if lease.Spec.Driver != "" && providerObject.Spec.Driver != lease.Spec.Driver {
		err := contracts.NewError(
			contracts.ErrorKindInvalid,
			"resolve allocation execution adapter",
			fmt.Sprintf("provider driver %q no longer matches Lease driver %q", providerObject.Spec.Driver, lease.Spec.Driver),
		)
		return r.failRuntimePreparation(ctx, workload, lease, "ExecutionDriverChanged", err)
	}
	plan, err := provider.BuildRuntimePlan(ctx, providerObject, pool, lease)
	if err != nil {
		fallbackEnabled, _, _, _ := fallbackSettings(workload)
		if fallbackEnabled && !podWasReady(workload) &&
			(contracts.IsErrorKind(err, contracts.ErrorKindConflict) || contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable)) {
			setCondition(&lease.Status.Conditions, lease.Generation, ConditionRuntimeReady, metav1.ConditionFalse, "RuntimePreparationFailed", err.Error(), r.now())
			if updateErr := r.updateLeaseStatus(ctx, lease); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return r.handlePrePodStartupFailure(ctx, workload, lease, startupFailure{Reason: "RuntimePreparationFailed", Message: err.Error()})
		}
		if isPermanentConfigurationError(err) {
			return r.failRuntimePreparation(ctx, workload, lease, "RuntimePlanRejected", err)
		}
		return ctrl.Result{}, err
	}
	managedResources, err := r.managedResources(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := validateRuntimePlan(plan, lease, managedResources); err != nil {
		return r.failRuntimePreparation(ctx, workload, lease, "RuntimePlanRejected", err)
	}
	mutatedTemplate, err := r.Injector.Mutate(workload.Spec.PodTemplate, plan)
	if err != nil {
		if isPermanentConfigurationError(err) {
			return r.failRuntimePreparation(ctx, workload, lease, "PodMutationRejected", err)
		}
		return ctrl.Result{}, err
	}
	pod := podFromTemplate(workload, lease, mutatedTemplate)
	if err := controllerutil.SetControllerReference(workload, pod, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if lease.Spec.Mode == acceleratorv1alpha1.ExecutionModeRemote {
		existing := &corev1.Pod{}
		if getErr := r.stateReader().Get(ctx, client.ObjectKeyFromObject(pod), existing); apierrors.IsNotFound(getErr) {
			healthy, message, err := r.remoteTargetsHealthy(ctx, lease)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !healthy {
				setCondition(&workload.Status.Conditions, workload.Generation, ConditionRuntimeReady, metav1.ConditionFalse, "EndpointUnavailable", message, r.now())
				return ctrl.Result{RequeueAfter: contracts.ObservationInterval}, r.updateWorkloadStatus(ctx, workload)
			}
		} else if getErr != nil {
			return ctrl.Result{}, getErr
		}
	}
	created := false
	if err := r.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// Timeouts and transport errors have an unknown commit result: the
			// Pod may already exist even though the response was lost. Keep the
			// reservation and retry the deterministic name. A later AlreadyExists
			// response will restore the bind safely. Only errors that prove the
			// API server rejected the object may release capacity immediately.
			if isPermanentPodCreateError(err) {
				return r.failRuntimePreparation(ctx, workload, lease, "PodCreateRejected", err)
			}
			return ctrl.Result{}, err
		}
		if err := r.stateReader().Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return ctrl.Result{}, err
		}
		if !metav1.IsControlledBy(pod, workload) || pod.Annotations["accelerator.qcrs.io/lease"] != lease.Name {
			conflictErr := contracts.NewError(contracts.ErrorKindInvalid, "create workload pod", fmt.Sprintf("pod %s/%s conflicts with accelerator workload", pod.Namespace, pod.Name))
			return r.failRuntimePreparation(ctx, workload, lease, "PodNameConflict", conflictErr)
		}
	} else {
		created = true
	}

	podRef := objectReferenceForPod(pod)
	if err := allocator.Bind(ctx, reservationRequest(workload, lease), *podRef); err != nil {
		return ctrl.Result{}, err
	}
	if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseBound); err != nil {
		return ctrl.Result{}, err
	}
	lease.Status.PodRef = podRef
	lease.Status.ExpiresAt = nil
	setCondition(&lease.Status.Conditions, lease.Generation, ConditionRuntimeReady, metav1.ConditionTrue, "Materialized", "runtime configuration was injected and the workload Pod was created", r.now())
	if err := r.updateLeaseStatus(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseStarting
	workload.Status.PodRef = podRef
	startedAt := pod.CreationTimestamp.Time
	if startedAt.IsZero() {
		startedAt = r.now()
	}
	startupStartedAt := metav1.NewTime(startedAt)
	workload.Status.Fallback.StartupStartedAt = &startupStartedAt
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionRuntimeReady, metav1.ConditionTrue, "Materialized", "runtime configuration was injected and the workload Pod was created", r.now())
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "Starting", "pod was created and is not ready yet", r.now())
	if err := r.updateWorkloadStatus(ctx, workload); err != nil {
		return ctrl.Result{}, err
	}
	if created {
		r.eventNormal(workload, "PodCreated", "created Pod %s for lease %s", pod.Name, lease.Name)
	}
	r.eventNormal(workload, "Bound", "bound lease %s to Pod %s", lease.Name, pod.Name)
	return ctrl.Result{Requeue: true}, nil
}

func (r *WorkloadReconciler) syncBoundPod(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Namespace: workload.Namespace, Name: podName(workload.Name, string(workload.UID))}
	if lease.Status.PodRef != nil {
		podKey.Name = lease.Status.PodRef.Name
	}
	if err := r.stateReader().Get(ctx, podKey, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if failureForAttempt(workload, workload.Status.AllocationAttempt) != nil {
			return r.continueStartupFailure(ctx, workload, lease)
		}
		if err := r.releaseLease(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "PodLost", "bound pod no longer exists", r.now())
		return ctrl.Result{}, r.updateWorkloadStatus(ctx, workload)
	}

	workload.Status.PodRef = objectReferenceForPod(pod)
	wasReady := podWasReady(workload)
	currentlyReady := podReady(pod)
	fallbackEnabled, _, startupTimeout, _ := fallbackSettings(workload)
	startedAt := startupStartedAt(workload, pod, r.now())
	if workload.Status.Fallback.StartupStartedAt == nil && !wasReady {
		value := metav1.NewTime(startedAt)
		workload.Status.Fallback.StartupStartedAt = &value
	}
	if lease.Spec.Mode == acceleratorv1alpha1.ExecutionModeRemote {
		wasDegraded := meta.IsStatusConditionTrue(workload.Status.Conditions, ConditionDegraded)
		healthy, message, err := r.remoteTargetsHealthy(ctx, lease)
		if err != nil {
			return ctrl.Result{}, err
		}
		if healthy {
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionDegraded, metav1.ConditionFalse, "EndpointsHealthy", message, r.now())
		} else {
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionDegraded, metav1.ConditionTrue, "EndpointUnavailable", message, r.now())
			if fallbackEnabled && !wasReady && !currentlyReady {
				return r.handleStartupFailure(ctx, workload, lease, pod, startupFailure{Reason: "EndpointUnavailable", Message: message})
			}
			if !wasDegraded {
				r.eventWarning(workload, "Degraded", "%s", message)
			}
		}
	}
	if fallbackEnabled && !wasReady && !currentlyReady {
		if failure, failed := detectStartupFailure(pod, startedAt, r.now(), startupTimeout); failed {
			return r.handleStartupFailure(ctx, workload, lease, pod, failure)
		}
	}
	result := ctrl.Result{}
	switch pod.Status.Phase {
	case corev1.PodRunning:
		ready := podReady(pod)
		status := metav1.ConditionFalse
		reason := "ContainersNotReady"
		message := "pod is running but not all containers are ready"
		if ready {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseRunning
			workload.Status.Fallback.StartupStartedAt = nil
			workload.Status.Fallback.ReadyObserved = true
			status = metav1.ConditionTrue
			reason = "Ready"
			message = "pod is running and ready"
		} else if wasReady {
			// ReadyObserved is a monotonic S2 boundary. Once crossed, temporary
			// readiness loss is degradation of a running attempt, not a return to
			// its initial startup state.
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseRunning
			reason = "PreviouslyReady"
			message = "pod reached Ready previously but is not currently ready"
		} else {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseStarting
		}
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, status, reason, message, r.now())
	case corev1.PodSucceeded, corev1.PodFailed:
		if err := r.releaseLease(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		if pod.Status.Phase == corev1.PodSucceeded {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseSucceeded
		} else {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
		}
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "Terminated", fmt.Sprintf("pod terminated with phase %s", pod.Status.Phase), r.now())
	default:
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseStarting
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "Starting", fmt.Sprintf("pod phase is %s", pod.Status.Phase), r.now())
	}
	if fallbackEnabled && !wasReady && workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseStarting {
		result = startupDeadlineResult(startedAt, r.now(), startupTimeout, result)
	}
	if lease.Spec.Mode == acceleratorv1alpha1.ExecutionModeRemote &&
		pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		if result.RequeueAfter == 0 || defaultEndpointCheckInterval < result.RequeueAfter {
			result.RequeueAfter = defaultEndpointCheckInterval
		}
	}
	if err := r.updateWorkloadStatus(ctx, workload); err != nil {
		return ctrl.Result{}, err
	}
	if !wasReady && podReady(pod) {
		r.eventNormal(workload, "Ready", "Pod %s became Ready in %s mode", pod.Name, lease.Spec.Mode)
	}
	return result, nil
}

func (r *WorkloadReconciler) syncTerminalStatus(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (ctrl.Result, error) {
	if workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseSucceeded || workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseFailed {
		return ctrl.Result{}, nil
	}
	if failureForAttempt(workload, workload.Status.AllocationAttempt) != nil {
		return r.continueStartupFailure(ctx, workload, lease)
	}
	if workload.Status.PodRef == nil {
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
		if condition := meta.FindStatusCondition(lease.Status.Conditions, ConditionRuntimeReady); condition != nil && condition.Status == metav1.ConditionFalse {
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionRuntimeReady, metav1.ConditionFalse, condition.Reason, condition.Message, r.now())
		} else {
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "PodNotCreated", "the allocation was released before a Pod was created", r.now())
		}
		return ctrl.Result{}, r.updateWorkloadStatus(ctx, workload)
	}
	pod := &corev1.Pod{}
	if err := r.stateReader().Get(ctx, types.NamespacedName{Namespace: workload.Namespace, Name: workload.Status.PodRef.Name}, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "PodLost", "released allocation pod no longer exists", r.now())
		return ctrl.Result{}, r.updateWorkloadStatus(ctx, workload)
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseSucceeded
	} else if pod.Status.Phase == corev1.PodFailed {
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
	}
	return ctrl.Result{}, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) failRuntimePreparation(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
	reason string,
	cause error,
) (ctrl.Result, error) {
	setCondition(&lease.Status.Conditions, lease.Generation, ConditionRuntimeReady, metav1.ConditionFalse, reason, cause.Error(), r.now())
	if err := r.updateLeaseStatus(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.releaseLease(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionRuntimeReady, metav1.ConditionFalse, reason, cause.Error(), r.now())
	return ctrl.Result{}, r.updateWorkloadStatus(ctx, workload)
}

func isPermanentConfigurationError(err error) bool {
	return contracts.IsErrorKind(err, contracts.ErrorKindInvalid) ||
		contracts.IsErrorKind(err, contracts.ErrorKindUnsupported)
}

func isPermanentPodCreateError(err error) bool {
	return apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) ||
		apierrors.IsForbidden(err) ||
		apierrors.IsUnauthorized(err) ||
		apierrors.IsMethodNotSupported(err)
}

func (r *WorkloadReconciler) expireLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Namespace: workload.Namespace, Name: podName(workload.Name, string(workload.UID))}
	if err := r.stateReader().Get(ctx, podKey, pod); err == nil {
		if metav1.IsControlledBy(pod, workload) && pod.Annotations["accelerator.qcrs.io/lease"] == lease.Name {
			if pod.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	allocator, err := r.allocatorForRelease(ctx, lease)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := allocator.Release(ctx, reservationRequest(workload, lease)); err != nil {
		return ctrl.Result{}, err
	}
	if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseExpired); err != nil {
		return ctrl.Result{}, err
	}
	setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionFalse, "Expired", "reservation expired before a pod was bound", r.now())
	if err := r.updateLeaseStatus(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.removeLeaseFinalizer(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	workload.Status.AllocationRef = ""
	workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
	return ctrl.Result{RequeueAfter: time.Second}, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) finalizeDeletingLease(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (ctrl.Result, error) {
	podPhase, waiting, err := r.deleteActiveLeasePod(ctx, workload, lease)
	if err != nil {
		return ctrl.Result{}, err
	}
	if waiting {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	statusChanged := false
	switch podPhase {
	case corev1.PodSucceeded:
		if workload.Status.Phase != acceleratorv1alpha1.WorkloadPhaseSucceeded {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseSucceeded
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "Terminated", "pod completed before its allocation lease was deleted", r.now())
			statusChanged = true
		}
	case corev1.PodFailed:
		if workload.Status.Phase != acceleratorv1alpha1.WorkloadPhaseFailed {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "Terminated", "pod failed before its allocation lease was deleted", r.now())
			statusChanged = true
		}
	default:
		if !workloadPhaseTerminal(workload.Status.Phase) {
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionFalse, "LeaseDeleted", "allocation lease was deleted before workload completion", r.now())
			statusChanged = true
		}
	}
	if statusChanged {
		if err := r.updateWorkloadStatus(ctx, workload); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.releaseLease(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *WorkloadReconciler) deleteActiveLeasePod(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (corev1.PodPhase, bool, error) {
	podKey := types.NamespacedName{Namespace: workload.Namespace, Name: podName(workload.Name, string(workload.UID))}
	if lease.Status.PodRef != nil && lease.Status.PodRef.Name != "" {
		podKey.Name = lease.Status.PodRef.Name
	}
	pod := &corev1.Pod{}
	if err := r.stateReader().Get(ctx, podKey, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}

	uidMatches := lease.Status.PodRef != nil && lease.Status.PodRef.UID != "" && lease.Status.PodRef.UID == pod.UID
	annotationMatches := pod.Annotations["accelerator.qcrs.io/lease"] == lease.Name
	if !metav1.IsControlledBy(pod, workload) || (!uidMatches && !annotationMatches) {
		return "", false, nil
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return pod.Status.Phase, false, nil
	}
	if pod.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return "", false, err
		}
	}
	return pod.Status.Phase, true, nil
}

func (r *WorkloadReconciler) releaseLease(ctx context.Context, lease *acceleratorv1alpha1.AcceleratorLease) error {
	if lease.Status.Phase == acceleratorv1alpha1.AllocationPhaseReleased {
		return r.removeLeaseFinalizer(ctx, lease)
	}
	if (lease.Status.Phase == acceleratorv1alpha1.AllocationPhaseFailed ||
		lease.Status.Phase == acceleratorv1alpha1.AllocationPhaseExpired) &&
		!controllerutil.ContainsFinalizer(lease, LeaseFinalizer) {
		// A terminal Lease without the release finalizer has already completed
		// its ownership cleanup (or never acquired capacity). Releasing it again
		// can conflict with a newer holder of the same Endpoint and wedge the
		// owning Workload finalizer.
		return nil
	}
	request := reservationRequestFromLease(lease)
	if lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseReleasing &&
		lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseFailed &&
		lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseExpired {
		if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseReleasing); err != nil {
			return err
		}
		if err := r.updateLeaseStatus(ctx, lease); err != nil {
			return err
		}
	}
	allocator, err := r.allocatorForRelease(ctx, lease)
	if err != nil {
		return err
	}
	if err := allocator.Release(ctx, request); err != nil {
		return err
	}
	if lease.Status.Phase == acceleratorv1alpha1.AllocationPhaseReleasing {
		if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseReleased); err != nil {
			return err
		}
		setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionFalse, "Released", "accelerator capacity was released", r.now())
		if err := r.updateLeaseStatus(ctx, lease); err != nil {
			return err
		}
		r.eventNormal(lease, "Released", "released accelerator capacity for pool %s", lease.Spec.PoolRef.Name)
	}
	return r.removeLeaseFinalizer(ctx, lease)
}

func (r *WorkloadReconciler) finalizeWorkload(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(workload, WorkloadFinalizer) {
		return ctrl.Result{}, nil
	}
	podKey := types.NamespacedName{Namespace: workload.Namespace, Name: podName(workload.Name, string(workload.UID))}
	if workload.Status.PodRef != nil {
		podKey.Name = workload.Status.PodRef.Name
	}
	pod := &corev1.Pod{}
	err := r.stateReader().Get(ctx, podKey, pod)
	if err == nil && metav1.IsControlledBy(pod, workload) {
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	leases, err := r.listWorkloadLeases(ctx, workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	for index := range leases {
		lease := &leases[index]
		if lease.Spec.Owner.UID != workload.UID {
			continue
		}
		if err := r.releaseLease(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(workload, WorkloadFinalizer)
	return ctrl.Result{}, r.Update(ctx, workload)
}

func (r *WorkloadReconciler) remoteTargetsHealthy(ctx context.Context, lease *acceleratorv1alpha1.AcceleratorLease) (bool, string, error) {
	for _, target := range lease.Spec.Targets {
		endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{}
		if err := r.stateReader().Get(ctx, types.NamespacedName{Name: target.EndpointName}, endpoint); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Sprintf("endpoint %s no longer exists", target.EndpointName), nil
			}
			return false, "", err
		}
		if endpoint.Spec.Unschedulable {
			return false, fmt.Sprintf("endpoint %s is unschedulable", target.EndpointName), nil
		}
		if condition := meta.FindStatusCondition(endpoint.Status.Conditions, ConditionConfigured); condition != nil {
			if condition.ObservedGeneration != endpoint.Generation {
				return false, fmt.Sprintf("endpoint %s Configured condition is stale for generation %d", target.EndpointName, endpoint.Generation), nil
			}
			if condition.Status != metav1.ConditionTrue {
				return false, fmt.Sprintf("endpoint %s Configured condition is %s: %s", target.EndpointName, condition.Status, condition.Message), nil
			}
		}
		if condition := meta.FindStatusCondition(endpoint.Status.Conditions, "Ready"); condition != nil {
			if condition.ObservedGeneration != endpoint.Generation {
				return false, fmt.Sprintf("endpoint %s Ready condition is stale for generation %d", target.EndpointName, endpoint.Generation), nil
			}
			if condition.Status != metav1.ConditionTrue {
				return false, fmt.Sprintf("endpoint %s Ready condition is %s: %s", target.EndpointName, condition.Status, condition.Message), nil
			}
		}
		if ready, reason := contracts.EndpointObservationCurrent(endpoint, time.Now()); !ready {
			return false, fmt.Sprintf("endpoint %s: %s", target.EndpointName, reason), nil
		}
		binding, found := boundEndpoint(lease.Status.Binding.Endpoints, target.EndpointName)
		if !found {
			return false, fmt.Sprintf("endpoint %s has no immutable binding snapshot", target.EndpointName), nil
		}
		if endpoint.Spec.PoolRef.Name != lease.Spec.PoolRef.Name {
			return false, fmt.Sprintf("endpoint %s moved to pool %s after the workload was bound", target.EndpointName, endpoint.Spec.PoolRef.Name), nil
		}
		if endpoint.Spec.Address != binding.Address {
			return false, fmt.Sprintf("endpoint %s address changed after the workload was bound", target.EndpointName), nil
		}
		if endpoint.Spec.Runtime != binding.Runtime {
			return false, fmt.Sprintf("endpoint %s runtime changed after the workload was bound", target.EndpointName), nil
		}
		if !stringMapsEqual(endpoint.Spec.Parameters, binding.Parameters) {
			return false, fmt.Sprintf("endpoint %s connection parameters changed after the workload was bound", target.EndpointName), nil
		}
		for _, deviceID := range target.DeviceIDs {
			if !healthyDeviceExists(endpoint.Spec.Devices, deviceID) {
				return false, fmt.Sprintf("endpoint %s device %s is no longer declared healthy", target.EndpointName, deviceID), nil
			}
		}
	}
	return true, "all selected remote endpoints remain configured and ready", nil
}

func boundEndpoint(endpoints []acceleratorv1alpha1.BoundEndpoint, name string) (acceleratorv1alpha1.BoundEndpoint, bool) {
	for _, endpoint := range endpoints {
		if endpoint.Name == name {
			return endpoint, true
		}
	}
	return acceleratorv1alpha1.BoundEndpoint{}, false
}

func healthyDeviceExists(devices []acceleratorv1alpha1.DeviceInfo, id string) bool {
	for _, device := range devices {
		if device.ID == id {
			return device.Health == acceleratorv1alpha1.DeviceHealthHealthy
		}
	}
	return false
}

func stringMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
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

func podFromTemplate(
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
	template corev1.PodTemplateSpec,
) *corev1.Pod {
	labels := copyMap(template.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	for key, value := range workloadIdentityLabels(workload) {
		labels[key] = value
	}
	annotations := copyMap(template.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["accelerator.qcrs.io/lease"] = lease.Name
	annotations[workloadNameAnnotation] = workload.Name
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName(workload.Name, string(workload.UID)),
			Namespace:   workload.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *template.Spec.DeepCopy(),
	}
}

func objectReferenceForPod(pod *corev1.Pod) *corev1.ObjectReference {
	return &corev1.ObjectReference{
		APIVersion: "v1",
		Kind:       "Pod",
		Namespace:  pod.Namespace,
		Name:       pod.Name,
		UID:        pod.UID,
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
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

func (r *WorkloadReconciler) listWorkloadLeases(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload) ([]acceleratorv1alpha1.AcceleratorLease, error) {
	byName := map[string]acceleratorv1alpha1.AcceleratorLease{}
	current := &acceleratorv1alpha1.AcceleratorLeaseList{}
	identity := workloadIdentityLabels(workload)[workloadUIDLabel]
	if err := r.stateReader().List(ctx, current, client.InNamespace(workload.Namespace), client.MatchingLabels{workloadUIDLabel: identity}); err != nil {
		return nil, err
	}
	for index := range current.Items {
		byName[current.Items[index].Name] = current.Items[index]
	}
	// Compatibility for Leases created before the UID label was introduced.
	if len(utilvalidation.IsValidLabelValue(workload.Name)) == 0 {
		legacy := &acceleratorv1alpha1.AcceleratorLeaseList{}
		if err := r.stateReader().List(ctx, legacy, client.InNamespace(workload.Namespace), client.MatchingLabels{workloadNameLabel: workload.Name}); err != nil {
			return nil, err
		}
		for index := range legacy.Items {
			byName[legacy.Items[index].Name] = legacy.Items[index]
		}
	}
	result := make([]acceleratorv1alpha1.AcceleratorLease, 0, len(byName))
	for _, lease := range byName {
		result = append(result, lease)
	}
	return result, nil
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
