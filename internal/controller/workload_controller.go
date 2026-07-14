package controller

import (
	"context"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	WorkloadFinalizer = "accelerator.qcrs.io/workload-protection"
	LeaseFinalizer    = "accelerator.qcrs.io/release-protection"

	ConditionAccepted = "Accepted"
	ConditionSelected = "Selected"
	ConditionReserved = "Reserved"
	ConditionPodReady = "PodReady"
	ConditionDegraded = "Degraded"

	defaultReservationTTL         = 5 * time.Minute
	unschedulableRetry            = 15 * time.Second
	defaultHeartbeatCheckInterval = 15 * time.Second
)

type WorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	Inventory contracts.InventoryReader
	Policy    contracts.PolicyEngine
	Allocator contracts.Allocator
	Providers *contracts.ProviderRegistry
	Injector  contracts.PodInjector
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
	if err := r.Get(ctx, request.NamespacedName, workload); err != nil {
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
		return r.selectAndCreateLease(ctx, workload)
	}

	lease := &acceleratorv1alpha1.AcceleratorLease{}
	leaseKey := types.NamespacedName{Namespace: workload.Namespace, Name: workload.Status.AllocationRef}
	if err := r.Get(ctx, leaseKey, lease); err != nil {
		if apierrors.IsNotFound(err) {
			workload.Status.AllocationRef = ""
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
			return ctrl.Result{Requeue: true}, r.updateWorkloadStatus(ctx, workload)
		}
		return ctrl.Result{}, err
	}
	if !lease.DeletionTimestamp.IsZero() {
		if err := r.releaseLease(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if allocation.IsExpired(lease, r.now()) {
		return r.expireLease(ctx, workload, lease)
	}

	switch lease.Status.Phase {
	case "":
		if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhasePending); err != nil {
			return ctrl.Result{}, err
		}
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
		return r.syncTerminalStatus(ctx, workload)
	case acceleratorv1alpha1.AllocationPhaseFailed, acceleratorv1alpha1.AllocationPhaseExpired:
		workload.Status.AllocationRef = ""
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
		return ctrl.Result{RequeueAfter: time.Second}, r.updateWorkloadStatus(ctx, workload)
	default:
		return ctrl.Result{}, fmt.Errorf("unknown allocation phase %q", lease.Status.Phase)
	}
}

func (r *WorkloadReconciler) selectAndCreateLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload) (ctrl.Result, error) {
	selectionRequest := contracts.SelectionRequest{
		Accelerator: workload.Spec.Accelerator,
		Placement:   workload.Spec.Placement,
		PodTemplate: workload.Spec.PodTemplate,
	}
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

	providerObject, pool, provider, err := r.resolveProvider(ctx, decision.ProviderName, decision.PoolName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := provider.Validate(providerObject, pool); err != nil {
		return ctrl.Result{}, err
	}

	attempt := workload.Status.AllocationAttempt + 1
	ttl := workload.Spec.Placement.ReservationTTL.Duration
	if ttl == 0 {
		ttl = defaultReservationTTL
	}
	lease := &acceleratorv1alpha1.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       leaseName(workload.Name, attempt),
			Namespace:  workload.Namespace,
			Finalizers: []string{LeaseFinalizer},
			Labels: map[string]string{
				"accelerator.qcrs.io/workload": workload.Name,
			},
		},
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			Owner: corev1.ObjectReference{
				APIVersion: acceleratorv1alpha1.GroupVersion.String(),
				Kind:       "AcceleratorWorkload",
				Namespace:  workload.Namespace,
				Name:       workload.Name,
				UID:        workload.UID,
			},
			Request:     workload.Spec.Accelerator,
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: decision.ProviderName},
			PoolRef:     acceleratorv1alpha1.ClusterObjectReference{Name: decision.PoolName},
			Mode:        decision.Mode,
			Targets:     decision.Targets,
			TTL:         metav1.Duration{Duration: ttl},
		},
	}
	if err := controllerutil.SetControllerReference(workload, lease, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, lease); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	if lease.UID == "" {
		if err := r.Get(ctx, client.ObjectKeyFromObject(lease), lease); err != nil {
			return ctrl.Result{}, err
		}
		if lease.Spec.Owner.UID != workload.UID {
			return ctrl.Result{}, fmt.Errorf("lease %s/%s is owned by a different workload", lease.Namespace, lease.Name)
		}
	}

	workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseReserving
	workload.Status.SelectedMode = decision.Mode
	workload.Status.AllocationRef = lease.Name
	workload.Status.AllocationAttempt = attempt
	workload.Status.Decision = &acceleratorv1alpha1.DecisionStatus{
		Mode:        decision.Mode,
		ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: decision.ProviderName},
		PoolRef:     acceleratorv1alpha1.ClusterObjectReference{Name: decision.PoolName},
		Targets:     decision.Targets,
		Policy:      decision.Policy,
		Reason:      decision.Reason,
		DecidedAt:   metav1.NewTime(r.now()),
	}
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionSelected, metav1.ConditionTrue, "Selected", decision.Reason, r.now())
	return ctrl.Result{Requeue: true}, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) reserveLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	request := reservationRequest(workload, lease)
	reservation, err := r.Allocator.Reserve(ctx, request)
	if err != nil {
		if contracts.IsErrorKind(err, contracts.ErrorKindConflict) || contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable) {
			_ = allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseFailed)
			setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionFalse, "CapacityChanged", err.Error(), r.now())
			if updateErr := r.updateLeaseStatus(ctx, lease); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			if finalizerErr := r.removeLeaseFinalizer(ctx, lease); finalizerErr != nil {
				return ctrl.Result{}, finalizerErr
			}
			workload.Status.AllocationRef = ""
			workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionFalse, "CapacityChanged", err.Error(), r.now())
			return ctrl.Result{RequeueAfter: time.Second}, r.updateWorkloadStatus(ctx, workload)
		}
		return ctrl.Result{}, err
	}
	if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseReserved); err != nil {
		return ctrl.Result{}, err
	}
	expiresAt := metav1.NewTime(reservation.ExpiresAt)
	lease.Status.Binding = acceleratorv1alpha1.RuntimeBinding{
		ID:        reservation.BindingID,
		Endpoints: append([]string(nil), reservation.Endpoints...),
	}
	lease.Status.ExpiresAt = &expiresAt
	setCondition(&lease.Status.Conditions, lease.Generation, ConditionReserved, metav1.ConditionTrue, "Reserved", "accelerator capacity is reserved", r.now())
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionTrue, "Reserved", "accelerator capacity is reserved", r.now())
	if err := r.updateLeaseStatus(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) ensurePod(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	providerObject, pool, provider, err := r.resolveProvider(ctx, lease.Spec.ProviderRef.Name, lease.Spec.PoolRef.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	plan, err := provider.BuildRuntimePlan(ctx, providerObject, pool, lease)
	if err != nil {
		return ctrl.Result{}, err
	}
	mutatedTemplate, err := r.Injector.Mutate(workload.Spec.PodTemplate, plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	pod := podFromTemplate(workload, lease, mutatedTemplate)
	if err := controllerutil.SetControllerReference(workload, pod, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			if releaseErr := r.releaseLease(ctx, lease); releaseErr != nil {
				return ctrl.Result{}, fmt.Errorf("create pod: %w; release reservation: %v", err, releaseErr)
			}
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return ctrl.Result{}, err
		}
		if !metav1.IsControlledBy(pod, workload) || pod.Annotations["accelerator.qcrs.io/lease"] != lease.Name {
			return ctrl.Result{}, fmt.Errorf("pod %s/%s conflicts with accelerator workload", pod.Namespace, pod.Name)
		}
	}

	podRef := objectReferenceForPod(pod)
	if err := r.Allocator.Bind(ctx, reservationRequest(workload, lease), *podRef); err != nil {
		return ctrl.Result{}, err
	}
	if err := allocation.Transition(lease, acceleratorv1alpha1.AllocationPhaseBound); err != nil {
		return ctrl.Result{}, err
	}
	lease.Status.PodRef = podRef
	lease.Status.ExpiresAt = nil
	if err := r.updateLeaseStatus(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseStarting
	workload.Status.PodRef = podRef
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "Starting", "pod was created and is not ready yet", r.now())
	return ctrl.Result{Requeue: true}, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) syncBoundPod(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Namespace: workload.Namespace, Name: podName(workload.Name, string(workload.UID))}
	if lease.Status.PodRef != nil {
		podKey.Name = lease.Status.PodRef.Name
	}
	if err := r.Get(ctx, podKey, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.releaseLease(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, "PodLost", "bound pod no longer exists", r.now())
		return ctrl.Result{}, r.updateWorkloadStatus(ctx, workload)
	}

	workload.Status.PodRef = objectReferenceForPod(pod)
	if lease.Spec.Mode == acceleratorv1alpha1.ExecutionModeRemote {
		healthy, message, err := r.remoteTargetsHealthy(ctx, lease)
		if err != nil {
			return ctrl.Result{}, err
		}
		if healthy {
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionDegraded, metav1.ConditionFalse, "EndpointsHealthy", message, r.now())
		} else {
			setCondition(&workload.Status.Conditions, workload.Generation, ConditionDegraded, metav1.ConditionTrue, "EndpointUnavailable", message, r.now())
		}
	}
	result := ctrl.Result{}
	switch pod.Status.Phase {
	case corev1.PodRunning:
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseRunning
		ready := podReady(pod)
		status := metav1.ConditionFalse
		reason := "ContainersNotReady"
		message := "pod is running but not all containers are ready"
		if ready {
			status = metav1.ConditionTrue
			reason = "Ready"
			message = "pod is running and ready"
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
	if lease.Spec.Mode == acceleratorv1alpha1.ExecutionModeRemote &&
		pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		result.RequeueAfter = defaultHeartbeatCheckInterval
	}
	return result, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) syncTerminalStatus(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload) (ctrl.Result, error) {
	if workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseSucceeded || workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseFailed {
		return ctrl.Result{}, nil
	}
	if workload.Status.PodRef == nil {
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
		return ctrl.Result{}, r.updateWorkloadStatus(ctx, workload)
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: workload.Namespace, Name: workload.Status.PodRef.Name}, pod); err != nil {
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

func (r *WorkloadReconciler) expireLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	if err := r.Allocator.Release(ctx, reservationRequest(workload, lease)); err != nil {
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

func (r *WorkloadReconciler) releaseLease(ctx context.Context, lease *acceleratorv1alpha1.AcceleratorLease) error {
	if lease.Status.Phase == "" {
		return r.removeLeaseFinalizer(ctx, lease)
	}
	if lease.Status.Phase == acceleratorv1alpha1.AllocationPhaseReleased {
		return r.removeLeaseFinalizer(ctx, lease)
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
	if err := r.Allocator.Release(ctx, request); err != nil {
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
	err := r.Get(ctx, podKey, pod)
	if err == nil {
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	leases := &acceleratorv1alpha1.AcceleratorLeaseList{}
	if err := r.List(ctx, leases, client.InNamespace(workload.Namespace), client.MatchingLabels{
		"accelerator.qcrs.io/workload": workload.Name,
	}); err != nil {
		return ctrl.Result{}, err
	}
	for index := range leases.Items {
		lease := &leases.Items[index]
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
		if err := r.Get(ctx, types.NamespacedName{Name: target.EndpointName}, endpoint); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Sprintf("endpoint %s no longer exists", target.EndpointName), nil
			}
			return false, "", err
		}
		if condition := meta.FindStatusCondition(endpoint.Status.Conditions, "AgentReady"); condition == nil || condition.Status != metav1.ConditionTrue {
			return false, fmt.Sprintf("endpoint %s is not AgentReady", target.EndpointName), nil
		}
		if endpoint.Status.HeartbeatLeaseRef == nil {
			return false, fmt.Sprintf("endpoint %s has no heartbeat Lease", target.EndpointName), nil
		}
		heartbeat := &coordinationv1.Lease{}
		key := types.NamespacedName{
			Namespace: endpoint.Status.HeartbeatLeaseRef.Namespace,
			Name:      endpoint.Status.HeartbeatLeaseRef.Name,
		}
		if err := r.Get(ctx, key, heartbeat); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Sprintf("endpoint %s heartbeat Lease is missing", target.EndpointName), nil
			}
			return false, "", err
		}
		if heartbeat.Spec.RenewTime == nil || heartbeat.Spec.LeaseDurationSeconds == nil {
			return false, fmt.Sprintf("endpoint %s heartbeat Lease is incomplete", target.EndpointName), nil
		}
		expiresAt := heartbeat.Spec.RenewTime.Add(time.Duration(*heartbeat.Spec.LeaseDurationSeconds) * time.Second)
		if !r.now().Before(expiresAt) {
			return false, fmt.Sprintf("endpoint %s heartbeat expired", target.EndpointName), nil
		}
	}
	return true, "all selected remote endpoints have current heartbeats", nil
}

func (r *WorkloadReconciler) resolveProvider(
	ctx context.Context,
	providerName,
	poolName string,
) (*acceleratorv1alpha1.AcceleratorProvider, *acceleratorv1alpha1.AcceleratorPool, contracts.Provider, error) {
	providerObject := &acceleratorv1alpha1.AcceleratorProvider{}
	if err := r.Get(ctx, types.NamespacedName{Name: providerName}, providerObject); err != nil {
		return nil, nil, nil, err
	}
	pool := &acceleratorv1alpha1.AcceleratorPool{}
	if err := r.Get(ctx, types.NamespacedName{Name: poolName}, pool); err != nil {
		return nil, nil, nil, err
	}
	if pool.Spec.ProviderRef.Name != providerName {
		return nil, nil, nil, fmt.Errorf("pool %q references provider %q instead of %q", poolName, pool.Spec.ProviderRef.Name, providerName)
	}
	provider, err := r.Providers.Get(providerObject.Spec.Driver)
	if err != nil {
		return nil, nil, nil, err
	}
	return providerObject, pool, provider, nil
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
	return r.Status().Update(ctx, workload)
}

func (r *WorkloadReconciler) updateLeaseStatus(ctx context.Context, lease *acceleratorv1alpha1.AcceleratorLease) error {
	lease.Status.ObservedGeneration = lease.Generation
	return r.Status().Update(ctx, lease)
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
		r.Allocator == nil || r.Providers == nil || r.Injector == nil {
		return fmt.Errorf("workload reconciler dependencies are incomplete")
	}
	return nil
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
	return contracts.ReservationRequest{
		LeaseKey:     string(lease.UID),
		WorkloadUID:  string(lease.Spec.Owner.UID),
		ProviderName: lease.Spec.ProviderRef.Name,
		PoolName:     lease.Spec.PoolRef.Name,
		Mode:         lease.Spec.Mode,
		Request:      lease.Spec.Request,
		Targets:      lease.Spec.Targets,
		TTL:          lease.Spec.TTL.Duration,
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
	labels["accelerator.qcrs.io/workload"] = workload.Name
	annotations := copyMap(template.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["accelerator.qcrs.io/lease"] = lease.Name
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
