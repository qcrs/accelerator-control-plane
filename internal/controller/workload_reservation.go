package controller

import (
	"context"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *WorkloadReconciler) reserveLease(ctx context.Context, workload *acceleratorv1alpha1.AcceleratorWorkload, lease *acceleratorv1alpha1.AcceleratorLease) (ctrl.Result, error) {
	// The Lease rejection may have committed before the Workload's failure
	// history conflicted. Restore that outcome instead of acquiring a Claim
	// again under a Lease whose recorded result says it acquired nothing.
	reserved := meta.FindStatusCondition(lease.Status.Conditions, ConditionReserved)
	if lease.Spec.Mode == acceleratorv1alpha1.ExecutionModeRemote && lease.Status.Binding.ID == "" &&
		reserved != nil && reserved.Status == metav1.ConditionFalse && reserved.Reason == "CapacityChanged" {
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionReserved, metav1.ConditionFalse, reserved.Reason, reserved.Message, r.now())
		return r.handlePrePodStartupFailure(ctx, workload, lease, startupFailure{Reason: reserved.Reason, Message: reserved.Message})
	}
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
