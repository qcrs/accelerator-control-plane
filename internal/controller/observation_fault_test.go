package controller

import (
	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func TestRemoteWorkloadWaitsForTrustworthyObservation(t *testing.T) {
	for _, state := range []string{"missing", "unknown", "unhealthy", "stale", "sessions"} {
		t.Run(state, func(t *testing.T) {
			h := newControllerHarness(t, "observe-"+state, nil, nil)
			w := h.workload(t)
			w.Spec.Placement = api.PlacementPolicy{Mode: api.PlacementModeRemoteOnly}
			if err := h.client.Update(h.ctx, w); err != nil {
				t.Fatal(err)
			}
			endpoint := &api.AcceleratorEndpoint{}
			if err := h.client.Get(h.ctx, client.ObjectKey{Name: "endpoint-a"}, endpoint); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "missing":
				endpoint.Status = api.AcceleratorEndpointStatus{}
			case "unknown":
				endpoint.Status.Health = api.DeviceHealthUnknown
			case "unhealthy":
				endpoint.Status.Health = api.DeviceHealthUnhealthy
			case "stale":
				at := metav1.NewTime(time.Now().Add(-2 * time.Minute))
				endpoint.Status.ObservedAt = &at
			case "sessions":
				active := int32(1)
				endpoint.Status.ActiveSessions = &active
			}
			if err := h.client.Status().Update(h.ctx, endpoint); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8; i++ {
				h.reconcile(t)
			}
			w = h.workload(t)
			if w.Status.PodRef != nil || w.Status.Phase != api.WorkloadPhasePending {
				t.Fatalf("unsafe endpoint got Pod: %+v", w.Status)
			}
			var pods corev1.PodList
			if err := h.client.List(h.ctx, &pods); err != nil {
				t.Fatal(err)
			}
			if len(pods.Items) != 0 {
				t.Fatal("unexpected Pod materialized")
			}
			endpoint.Status = observedEndpointStatusForTest(endpoint.Generation)
			if err := h.client.Status().Update(h.ctx, endpoint); err != nil {
				t.Fatal(err)
			}
			h.runUntilStarting(t, api.ExecutionModeRemote)
		})
	}
}

func TestEndpointFailureBetweenReservationAndPodDoesNotCreatePod(t *testing.T) {
	h := newControllerHarness(t, "observation-race", nil, nil)
	w := h.workload(t)
	w.Spec.Placement = api.PlacementPolicy{Mode: api.PlacementModeRemoteOnly}
	if err := h.client.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	var lease *api.AcceleratorLease
	for i := 0; i < 10; i++ {
		h.reconcile(t)
		var leases api.AcceleratorLeaseList
		if err := h.client.List(h.ctx, &leases); err != nil {
			t.Fatal(err)
		}
		for j := range leases.Items {
			if leases.Items[j].Status.Phase == api.AllocationPhaseReserved {
				lease = &leases.Items[j]
			}
		}
		if lease != nil {
			break
		}
	}
	if lease == nil {
		t.Fatal("did not reach Reserved")
	}
	endpoint := &api.AcceleratorEndpoint{}
	if err := h.client.Get(h.ctx, client.ObjectKey{Name: "endpoint-a"}, endpoint); err != nil {
		t.Fatal(err)
	}
	endpoint.Status.Health = api.DeviceHealthUnhealthy
	if err := h.client.Status().Update(h.ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	if h.workload(t).Status.PodRef != nil {
		t.Fatal("Pod created after endpoint became unhealthy")
	}
	var pods corev1.PodList
	h.client.List(h.ctx, &pods)
	if len(pods.Items) != 0 {
		t.Fatal("unexpected Pod")
	}
	if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(lease), &api.AcceleratorLease{}); err != nil {
		t.Fatal("existing reservation deleted", err)
	}
}
