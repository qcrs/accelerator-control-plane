package controller

import (
	"context"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
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

	ConditionAccepted     = "Accepted"
	ConditionSelected     = "Selected"
	ConditionReserved     = "Reserved"
	ConditionRuntimeReady = "RuntimeReady"
	ConditionPodReady     = "PodReady"
	ConditionDegraded     = "Degraded"

	decisionPolicyAnnotation = "accelerator.qcrs.io/decision-policy"
	decisionReasonAnnotation = "accelerator.qcrs.io/decision-reason"

	defaultReservationTTL        = 5 * time.Minute
	unschedulableRetry           = 15 * time.Second
	defaultEndpointCheckInterval = 15 * time.Second
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
	existingKey := types.NamespacedName{Namespace: workload.Namespace, Name: leaseName(workload.Name, attempt)}
	if err := r.Get(ctx, existingKey, existing); err == nil {
		return r.restoreLeaseSelection(ctx, workload, existing, attempt)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

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
			Name:       leaseName(workload.Name, attempt),
			Namespace:  workload.Namespace,
			Finalizers: []string{LeaseFinalizer},
			Annotations: map[string]string{
				decisionPolicyAnnotation: decision.Policy,
				decisionReasonAnnotation: decision.Reason,
			},
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
			WorkloadGeneration: workload.Generation,
			Request:            workload.Spec.Accelerator,
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
		if err := r.Get(ctx, client.ObjectKeyFromObject(lease), existing); err != nil {
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
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionSelected, metav1.ConditionTrue, "Selected", reason, r.now())
	return ctrl.Result{Requeue: true}, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) reserveLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	request := reservationRequest(workload, lease)
	reservation, err := r.Allocator.Reserve(ctx, request)
	if err != nil {
		reason := ""
		switch {
		case contracts.IsErrorKind(err, contracts.ErrorKindConflict), contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable):
			reason = "CapacityChanged"
		case contracts.IsErrorKind(err, contracts.ErrorKindInvalid), contracts.IsErrorKind(err, contracts.ErrorKindUnsupported):
			reason = "ReservationRejected"
		}
		if reason != "" {
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
	return ctrl.Result{Requeue: true}, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) ensurePod(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	providerObject, pool, provider, err := r.resolveProvider(ctx, lease.Spec.ProviderRef.Name, lease.Spec.PoolRef.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	plan, err := provider.BuildRuntimePlan(ctx, providerObject, pool, lease)
	if err != nil {
		if isPermanentConfigurationError(err) {
			return r.failRuntimePreparation(ctx, workload, lease, "RuntimePlanRejected", err)
		}
		return ctrl.Result{}, err
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
			conflictErr := contracts.NewError(contracts.ErrorKindInvalid, "create workload pod", fmt.Sprintf("pod %s/%s conflicts with accelerator workload", pod.Namespace, pod.Name))
			return r.failRuntimePreparation(ctx, workload, lease, "PodNameConflict", conflictErr)
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
		result.RequeueAfter = defaultEndpointCheckInterval
	}
	return result, r.updateWorkloadStatus(ctx, workload)
}

func (r *WorkloadReconciler) syncTerminalStatus(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (ctrl.Result, error) {
	if workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseSucceeded || workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseFailed {
		return ctrl.Result{}, nil
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

func (r *WorkloadReconciler) expireLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Namespace: workload.Namespace, Name: podName(workload.Name, string(workload.UID))}
	if err := r.Get(ctx, podKey, pod); err == nil {
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
	if err := r.Get(ctx, podKey, pod); err != nil {
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
		if endpoint.Spec.Unschedulable {
			return false, fmt.Sprintf("endpoint %s is unschedulable", target.EndpointName), nil
		}
		if condition := meta.FindStatusCondition(endpoint.Status.Conditions, "Ready"); condition != nil && condition.Status != metav1.ConditionTrue {
			return false, fmt.Sprintf("endpoint %s Ready condition is %s: %s", target.EndpointName, condition.Status, condition.Message), nil
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

func workloadPhaseTerminal(phase acceleratorv1alpha1.WorkloadPhase) bool {
	return phase == acceleratorv1alpha1.WorkloadPhaseSucceeded || phase == acceleratorv1alpha1.WorkloadPhaseFailed
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
