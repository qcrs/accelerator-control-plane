package controller

import (
	"context"
	"fmt"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

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
