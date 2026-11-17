package controller

import (
	"context"
	"fmt"
	"strings"
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

const (
	ConditionFallback = "Fallback"

	defaultFallbackMaxAttempts    int32 = 2
	defaultFallbackStartupTimeout       = 5 * time.Minute
	defaultFallbackBackoff              = 5 * time.Second
)

type startupFailure struct {
	Reason  string
	Message string
}

const maxFailureMessageRunes = 2048

func fallbackSettings(workload *acceleratorv1alpha1.AcceleratorWorkload) (bool, int32, time.Duration, time.Duration) {
	if workload == nil || workload.Spec.Placement.Fallback == nil || !workload.Spec.Placement.Fallback.Enabled {
		return false, 0, 0, 0
	}
	policy := workload.Spec.Placement.Fallback
	maxAttempts := policy.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultFallbackMaxAttempts
	}
	startupTimeout := policy.StartupTimeout.Duration
	if startupTimeout == 0 {
		startupTimeout = defaultFallbackStartupTimeout
	}
	backoff := policy.Backoff.Duration
	if backoff == 0 {
		backoff = defaultFallbackBackoff
	}
	return true, maxAttempts, startupTimeout, backoff
}

func (r *WorkloadReconciler) waitForFallback(workload *acceleratorv1alpha1.AcceleratorWorkload) (ctrl.Result, bool) {
	if workload.Status.Fallback.RequeueAt == nil {
		return ctrl.Result{}, false
	}
	delay := workload.Status.Fallback.RequeueAt.Time.Sub(r.now())
	if delay <= 0 {
		return ctrl.Result{}, false
	}
	return ctrl.Result{RequeueAfter: delay}, true
}

func fallbackSelectionContext(workload *acceleratorv1alpha1.AcceleratorWorkload) (acceleratorv1alpha1.ExecutionMode, []contracts.CandidateExclusion) {
	history := workload.Status.Fallback.History
	if len(history) == 0 {
		return "", nil
	}
	lastMode := history[len(history)-1].Mode
	preferred := acceleratorv1alpha1.ExecutionModeLocal
	if lastMode == acceleratorv1alpha1.ExecutionModeLocal {
		preferred = acceleratorv1alpha1.ExecutionModeRemote
	}
	exclusions := make([]contracts.CandidateExclusion, 0, len(history))
	for _, failure := range history {
		for _, target := range failure.Targets {
			targetName := target.NodeName
			if failure.Mode == acceleratorv1alpha1.ExecutionModeRemote {
				targetName = target.EndpointName
			}
			if targetName == "" {
				continue
			}
			exclusions = append(exclusions, contracts.CandidateExclusion{
				Mode:       failure.Mode,
				PoolName:   failure.PoolRef.Name,
				TargetName: targetName,
			})
		}
	}
	return preferred, exclusions
}

func startupStartedAt(workload *acceleratorv1alpha1.AcceleratorWorkload, pod *corev1.Pod, now time.Time) time.Time {
	if workload.Status.Fallback.StartupStartedAt != nil && !workload.Status.Fallback.StartupStartedAt.IsZero() {
		return workload.Status.Fallback.StartupStartedAt.Time
	}
	if pod != nil && !pod.CreationTimestamp.IsZero() {
		return pod.CreationTimestamp.Time
	}
	return now
}

func detectStartupFailure(pod *corev1.Pod, startedAt, now time.Time, timeout time.Duration) (startupFailure, bool) {
	if pod == nil {
		return startupFailure{}, false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
			message := condition.Message
			if message == "" {
				message = "pod cannot be scheduled on the selected target"
			}
			return startupFailure{Reason: "Unschedulable", Message: message}, true
		}
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if !managedRuntimeInitContainer(pod, status.Name) {
			continue
		}
		if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
			return startupFailure{
				Reason:  "InitContainerFailed",
				Message: fmt.Sprintf("init container %s exited with code %d", status.Name, status.State.Terminated.ExitCode),
			}, true
		}
		if status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
			return startupFailure{Reason: "InitContainerFailed", Message: containerWaitingMessage("init container", status.Name, status.State.Waiting)}, true
		}
		if failure, found := waitingStartupFailure("init container", status.Name, status.State.Waiting); found {
			return failure, true
		}
	}
	if timeout > 0 && !startedAt.IsZero() && !now.Before(startedAt.Add(timeout)) {
		return startupFailure{
			Reason:  "StartupTimeout",
			Message: fmt.Sprintf("pod did not become Ready within %s", timeout),
		}, true
	}
	return startupFailure{}, false
}

func managedRuntimeInitContainer(pod *corev1.Pod, name string) bool {
	if pod == nil || name == "" {
		return false
	}
	for _, managedName := range strings.Split(pod.Annotations[contracts.RuntimeInitContainersAnnotation], ",") {
		if strings.TrimSpace(managedName) == name {
			return true
		}
	}
	return false
}

func waitingStartupFailure(kind, name string, waiting *corev1.ContainerStateWaiting) (startupFailure, bool) {
	if waiting == nil {
		return startupFailure{}, false
	}
	switch waiting.Reason {
	case "ErrImagePull", "ImagePullBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
		return startupFailure{Reason: waiting.Reason, Message: containerWaitingMessage(kind, name, waiting)}, true
	default:
		return startupFailure{}, false
	}
}

func containerWaitingMessage(kind, name string, waiting *corev1.ContainerStateWaiting) string {
	if waiting.Message == "" {
		return fmt.Sprintf("%s %s is waiting: %s", kind, name, waiting.Reason)
	}
	return fmt.Sprintf("%s %s is waiting: %s: %s", kind, name, waiting.Reason, waiting.Message)
}

func failureForAttempt(workload *acceleratorv1alpha1.AcceleratorWorkload, attempt int32) *acceleratorv1alpha1.StartupFailureStatus {
	for index := range workload.Status.Fallback.History {
		if workload.Status.Fallback.History[index].Attempt == attempt {
			return &workload.Status.Fallback.History[index]
		}
	}
	return nil
}

func (r *WorkloadReconciler) handleStartupFailure(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
	pod *corev1.Pod,
	failure startupFailure,
) (ctrl.Result, error) {
	return r.recordStartupFailure(ctx, workload, lease, pod, failure)
}

func (r *WorkloadReconciler) handlePrePodStartupFailure(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
	failure startupFailure,
) (ctrl.Result, error) {
	return r.recordStartupFailure(ctx, workload, lease, nil, failure)
}

func (r *WorkloadReconciler) recordStartupFailure(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
	pod *corev1.Pod,
	failure startupFailure,
) (ctrl.Result, error) {
	if failureForAttempt(workload, workload.Status.AllocationAttempt) == nil {
		now := r.now()
		failure.Message = boundedFailureMessage(failure.Message)
		startedAt := startupStartedAt(workload, pod, now)
		var podRef *corev1.ObjectReference
		if pod != nil {
			podRef = objectReferenceForPod(pod)
		}
		workload.Status.Fallback.History = append(workload.Status.Fallback.History, acceleratorv1alpha1.StartupFailureStatus{
			Attempt:     workload.Status.AllocationAttempt,
			Mode:        lease.Spec.Mode,
			ProviderRef: lease.Spec.ProviderRef,
			PoolRef:     lease.Spec.PoolRef,
			Targets:     copyAllocationTargets(lease.Spec.Targets),
			PodRef:      podRef,
			StartedAt:   metav1.NewTime(startedAt),
			FailedAt:    metav1.NewTime(now),
			Reason:      failure.Reason,
			Message:     failure.Message,
		})
		workload.Status.Fallback.FailedAttempts++
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseTerminating
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionPodReady, metav1.ConditionFalse, failure.Reason, failure.Message, now)
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionFallback, metav1.ConditionTrue, "CleanupPending", "cleaning up the failed startup attempt before releasing its allocation", now)
		if err := r.updateWorkloadStatus(ctx, workload); err != nil {
			return ctrl.Result{}, err
		}
		r.eventWarning(workload, "StartupFailed", "attempt %d in %s mode failed before Ready: %s", workload.Status.AllocationAttempt, lease.Spec.Mode, failure.Message)
		return ctrl.Result{Requeue: true}, nil
	}
	return r.continueStartupFailure(ctx, workload, lease)
}

func boundedFailureMessage(message string) string {
	value := []rune(message)
	if len(value) <= maxFailureMessageRunes {
		return message
	}
	return string(value[:maxFailureMessageRunes])
}

func (r *WorkloadReconciler) continueStartupFailure(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (ctrl.Result, error) {
	failure := failureForAttempt(workload, workload.Status.AllocationAttempt)
	if failure != nil && failure.PodRef == nil {
		if err := r.releaseLease(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		return r.finishStartupFailure(ctx, workload, lease)
	}
	podKey := types.NamespacedName{Namespace: workload.Namespace, Name: podName(workload.Name, string(workload.UID))}
	if lease.Status.PodRef != nil && lease.Status.PodRef.Name != "" {
		podKey.Name = lease.Status.PodRef.Name
	} else if workload.Status.PodRef != nil && workload.Status.PodRef.Name != "" {
		podKey.Name = workload.Status.PodRef.Name
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, podKey, pod); err == nil {
		uidMatches := lease.Status.PodRef != nil && lease.Status.PodRef.UID != "" && lease.Status.PodRef.UID == pod.UID
		annotationMatches := pod.Annotations["accelerator.qcrs.io/lease"] == lease.Name
		if !metav1.IsControlledBy(pod, workload) || (!uidMatches && !annotationMatches) {
			return ctrl.Result{}, contracts.NewError(contracts.ErrorKindConflict, "clean up failed startup", fmt.Sprintf("pod %s/%s is not owned by allocation %s", pod.Namespace, pod.Name, lease.Name))
		}
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if err := r.releaseLease(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	return r.finishStartupFailure(ctx, workload, lease)
}

func (r *WorkloadReconciler) finishStartupFailure(
	ctx context.Context,
	workload *acceleratorv1alpha1.AcceleratorWorkload,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (ctrl.Result, error) {
	enabled, maxAttempts, _, backoff := fallbackSettings(workload)
	workload.Status.Fallback.StartupStartedAt = nil
	workload.Status.PodRef = nil
	if enabled && workload.Status.Fallback.FailedAttempts < maxAttempts {
		now := r.now()
		requeueAt := metav1.NewTime(now.Add(backoff))
		workload.Status.Fallback.RequeueAt = &requeueAt
		workload.Status.AllocationRef = ""
		workload.Status.SelectedMode = ""
		workload.Status.Decision = nil
		workload.Status.Phase = acceleratorv1alpha1.WorkloadPhasePending
		message := fmt.Sprintf("startup attempt %d was released; next selection is eligible at %s", workload.Status.AllocationAttempt, requeueAt.Time.UTC().Format(time.RFC3339))
		setCondition(&workload.Status.Conditions, workload.Generation, ConditionFallback, metav1.ConditionTrue, "Scheduled", message, now)
		if err := r.updateWorkloadStatus(ctx, workload); err != nil {
			return ctrl.Result{}, err
		}
		r.eventNormal(workload, "FallbackScheduled", "%s", message)
		return ctrl.Result{RequeueAfter: backoff}, nil
	}

	workload.Status.Fallback.RequeueAt = nil
	workload.Status.Phase = acceleratorv1alpha1.WorkloadPhaseFailed
	message := fmt.Sprintf("startup fallback exhausted after %d failed attempt(s)", workload.Status.Fallback.FailedAttempts)
	setCondition(&workload.Status.Conditions, workload.Generation, ConditionFallback, metav1.ConditionFalse, "Exhausted", message, r.now())
	if err := r.updateWorkloadStatus(ctx, workload); err != nil {
		return ctrl.Result{}, err
	}
	r.eventWarning(workload, "FallbackExhausted", "%s; last allocation was %s", message, lease.Name)
	return ctrl.Result{}, nil
}

func startupDeadlineResult(startedAt, now time.Time, timeout time.Duration, current ctrl.Result) ctrl.Result {
	if timeout <= 0 || startedAt.IsZero() {
		return current
	}
	delay := startedAt.Add(timeout).Sub(now)
	if delay <= 0 {
		delay = time.Second
	}
	if current.RequeueAfter == 0 || delay < current.RequeueAfter {
		current.RequeueAfter = delay
	}
	return current
}

func podWasReady(workload *acceleratorv1alpha1.AcceleratorWorkload) bool {
	return workload.Status.Fallback.ReadyObserved || meta.IsStatusConditionTrue(workload.Status.Conditions, ConditionPodReady)
}
