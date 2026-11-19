package controller

import (
	"context"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

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
