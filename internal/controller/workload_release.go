package controller

import (
	"context"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

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
	// A non-transient Reserve rejection left no Claim for this Lease: Claim
	// either lost the competition or the allocator confirmed its rollback.
	// Persisted conditions carry this result across controller restarts. A
	// ReservationRetrying result remains uncertain and must still be released.
	reserved := meta.FindStatusCondition(lease.Status.Conditions, ConditionReserved)
	unacquiredRemote := lease.Spec.Mode == acceleratorv1alpha1.ExecutionModeRemote &&
		lease.Status.Binding.ID == "" && reserved != nil && reserved.Status == metav1.ConditionFalse && reserved.Reason == "CapacityChanged"
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
	if !unacquiredRemote {
		allocator, err := r.allocatorForRelease(ctx, lease)
		if err != nil {
			return err
		}
		if err := allocator.Release(ctx, request); err != nil {
			return err
		}
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
