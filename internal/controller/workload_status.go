package controller

import (
	"context"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

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
	terminal := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
	fallbackEnabled, _, startupTimeout, _ := fallbackSettings(workload)
	startedAt := startupStartedAt(workload, pod, r.now())
	if workload.Status.Fallback.StartupStartedAt == nil && !wasReady {
		value := metav1.NewTime(startedAt)
		workload.Status.Fallback.StartupStartedAt = &value
	}
	// A completed application's result takes precedence over current endpoint
	// health. Short jobs may finish between reconciles without ever being Ready.
	if lease.Spec.Mode == acceleratorv1alpha1.ExecutionModeRemote && !terminal {
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
		workload.Status.Fallback.StartupStartedAt = nil
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
