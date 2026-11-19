package controller

import (
	"testing"
	"time"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestCompletedPodBeforeReadyDoesNotFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        api.ExecutionMode
		phase       corev1.PodPhase
		expired     bool
		unavailable bool
	}{
		{"quick local success", api.ExecutionModeLocal, corev1.PodSucceeded, false, false},
		{"success observed after startup deadline", api.ExecutionModeLocal, corev1.PodSucceeded, true, false},
		{"remote success after endpoint outage", api.ExecutionModeRemote, corev1.PodSucceeded, false, true},
		{"application failure after deadline and outage", api.ExecutionModeRemote, corev1.PodFailed, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := []api.ExecutionMode{api.ExecutionModeLocal, api.ExecutionModeRemote}
			if tc.mode == api.ExecutionModeRemote {
				order[0], order[1] = order[1], order[0]
			}
			h := newControllerHarness(t, "completed-before-ready", order, nil)
			w := h.runUntilStarting(t, tc.mode)
			originalLease, originalAttempt := w.Status.AllocationRef, w.Status.AllocationAttempt
			pod := h.pod(t, w)
			pod.Status.Phase = tc.phase
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
			exitCode := int32(0)
			if tc.phase == corev1.PodFailed {
				exitCode = 2
			}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode},
			}}}
			if err := h.client.Status().Update(h.ctx, pod); err != nil {
				t.Fatal(err)
			}
			if tc.expired {
				h.now = h.now.Add(10 * time.Minute)
			}
			if tc.unavailable {
				endpoint := &api.AcceleratorEndpoint{}
				if err := h.client.Get(h.ctx, client.ObjectKey{Name: "endpoint-a"}, endpoint); err != nil {
					t.Fatal(err)
				}
				endpoint.Spec.Unschedulable = true
				if err := h.client.Update(h.ctx, endpoint); err != nil {
					t.Fatal(err)
				}
			}
			h.reconcile(t)
			w = h.workload(t)
			want := api.WorkloadPhaseSucceeded
			if tc.phase == corev1.PodFailed {
				want = api.WorkloadPhaseFailed
			}
			if w.Status.Phase != want || len(w.Status.Fallback.History) != 0 || w.Status.Fallback.FailedAttempts != 0 {
				t.Fatalf("completed application was classified as a startup failure: %+v", w.Status)
			}
			for i := 0; i < 3; i++ {
				h.reconcile(t)
			}
			w = h.workload(t)
			if w.Status.AllocationRef != originalLease || w.Status.AllocationAttempt != originalAttempt || w.Status.Fallback.ReadyObserved {
				t.Fatalf("terminal result was replayed or Ready was fabricated: %+v", w.Status)
			}
			lease := &api.AcceleratorLease{}
			if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: w.Namespace, Name: originalLease}, lease); err != nil {
				t.Fatal(err)
			}
			if lease.Status.Phase != api.AllocationPhaseReleased || controllerutil.ContainsFinalizer(lease, LeaseFinalizer) {
				t.Fatalf("terminal application did not release its allocation: %+v", lease.Status)
			}
			if h.pod(t, w).DeletionTimestamp != nil || h.claimer.holders["endpoint-a"] != "" {
				t.Fatal("completed Pod was deleted or remote ownership was retained")
			}
		})
	}
}

func TestFailedManagedRuntimeInitBeforeReadyStillFallsBack(t *testing.T) {
	h := newControllerHarness(t, "terminal-init-failure", []api.ExecutionMode{api.ExecutionModeRemote, api.ExecutionModeLocal}, nil)
	w := h.runUntilStarting(t, api.ExecutionModeRemote)
	pod := h.pod(t, w)
	pod.Status.Phase = corev1.PodFailed
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "accelerator-runtime-setup", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
	}}}
	if err := h.client.Status().Update(h.ctx, pod); err != nil {
		t.Fatal(err)
	}
	w = h.runUntilStarting(t, api.ExecutionModeLocal)
	if len(w.Status.Fallback.History) != 1 || w.Status.Fallback.History[0].Reason != "InitContainerFailed" {
		t.Fatalf("terminal init failure lost the existing startup fallback: %+v", w.Status)
	}
}
