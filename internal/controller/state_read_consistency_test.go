package controller

import (
	"context"
	"io"
	"testing"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	cricketprovider "github.com/qcrs/accelerator-control-plane/internal/provider/cricket"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type probeFailureOnce struct {
	contracts.RemoteEndpointValidator
	calls int
}

func (p *probeFailureOnce) Observe(ctx context.Context, endpoint contracts.RemoteEndpoint) (contracts.EndpointObservation, error) {
	p.calls++
	if p.calls == 1 {
		return contracts.EndpointObservation{}, io.ErrUnexpectedEOF
	}
	return p.RemoteEndpointValidator.(contracts.EndpointObserver).Observe(ctx, endpoint)
}

func pendingReservationForTest(t *testing.T, h *controllerHarness) (*api.AcceleratorWorkload, *api.AcceleratorLease) {
	t.Helper()
	for i := 0; i < 8; i++ {
		h.reconcile(t)
		w := h.workload(t)
		if w.Status.AllocationRef == "" {
			continue
		}
		lease := &api.AcceleratorLease{}
		if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Status.AllocationRef}, lease); err != nil {
			t.Fatal(err)
		}
		if lease.Status.Phase == api.AllocationPhasePending {
			return w, lease
		}
	}
	t.Fatal("Pending reservation was not reached")
	return nil, nil
}

func TestProbeEOFDoesNotConsumeFallbackOrExcludeOnlyEndpoint(t *testing.T) {
	h := newControllerHarness(t, "probe-retry", []api.ExecutionMode{api.ExecutionModeRemote, api.ExecutionModeLocal}, nil)
	localPool := &api.AcceleratorPool{}
	if err := h.client.Get(h.ctx, client.ObjectKey{Name: "local"}, localPool); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(h.ctx, localPool); err != nil {
		t.Fatal(err)
	}
	execution, _ := h.adapters.Get(cricketprovider.DriverName)
	allocator := execution.Allocator().(*allocation.RemoteAllocator)
	probe := &probeFailureOnce{RemoteEndpointValidator: allocator.Validator}
	allocator.Validator = probe
	w, _ := pendingReservationForTest(t, h)
	ref := w.Status.AllocationRef
	_, err := h.reconciler.Reconcile(h.ctx, h.request)
	if !contracts.IsErrorKind(err, contracts.ErrorKindTransient) {
		t.Fatalf("probe transport failure was counted as startup failure: %v", err)
	}
	w = h.workload(t)
	if w.Status.AllocationRef != ref || w.Status.PodRef != nil || len(w.Status.Fallback.History) != 0 || w.Status.Fallback.FailedAttempts != 0 {
		t.Fatalf("temporary observation excluded the only usable endpoint: %+v", w.Status)
	}
	h.reconciler = h.newReconciler()
	w = h.runUntilStarting(t, api.ExecutionModeRemote)
	if w.Status.AllocationRef != ref || w.Status.AllocationAttempt != 1 || probe.calls != 2 {
		t.Fatalf("recovery did not reuse the same reservation: %+v probes=%d", w.Status, probe.calls)
	}
}

type staleLifecycleCache struct {
	client.Client
	workload *api.AcceleratorWorkload
	lease    *api.AcceleratorLease
}

func (c *staleLifecycleCache) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	switch target := object.(type) {
	case *api.AcceleratorWorkload:
		if key.Name == c.workload.Name {
			*target = *c.workload.DeepCopy()
			return nil
		}
	case *api.AcceleratorLease:
		if key.Name == c.lease.Name {
			*target = *c.lease.DeepCopy()
			return nil
		}
	}
	return c.Client.Get(ctx, key, object, options...)
}

func TestStalePendingCacheCannotRepeatReservationAfterPodExists(t *testing.T) {
	h := newControllerHarness(t, "cache-state", []api.ExecutionMode{api.ExecutionModeRemote, api.ExecutionModeLocal}, nil)
	w, lease := pendingReservationForTest(t, h)
	stale := &staleLifecycleCache{Client: h.client, workload: w.DeepCopy(), lease: lease.DeepCopy()}
	w = h.runUntilStarting(t, api.ExecutionModeRemote)
	pod := h.pod(t, w)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := h.client.Status().Update(h.ctx, pod); err != nil {
		t.Fatal(err)
	}
	execution, _ := h.adapters.Get(cricketprovider.DriverName)
	allocator := execution.Allocator().(*allocation.RemoteAllocator)
	probe := &probeFailureOnce{RemoteEndpointValidator: allocator.Validator}
	allocator.Validator = probe
	h.reconciler.Client = stale
	h.reconciler.APIReader = h.client
	_, err := h.reconciler.Reconcile(h.ctx, h.request)
	if err != nil {
		t.Fatal(err)
	}
	w = h.workload(t)
	if probe.calls != 0 || w.Status.Phase != api.WorkloadPhaseRunning || !w.Status.Fallback.ReadyObserved ||
		w.Status.AllocationRef != lease.Name || len(w.Status.Fallback.History) != 0 {
		t.Fatalf("stale Pending state repeated the ownership transaction: probes=%d status=%+v", probe.calls, w.Status)
	}
}

func TestStaleStatusSnapshotCannotOverwriteCommittedReady(t *testing.T) {
	h := newControllerHarness(t, "status-precondition", []api.ExecutionMode{api.ExecutionModeRemote}, nil)
	stale := h.workload(t)
	current := stale.DeepCopy()
	current.Status.Fallback.ReadyObserved = true
	if err := h.client.Status().Update(h.ctx, current); err != nil {
		t.Fatal(err)
	}
	stale.Status.Phase = api.WorkloadPhasePending
	if err := h.reconciler.updateWorkloadStatus(h.ctx, stale); !apierrors.IsConflict(err) {
		t.Fatalf("stale status write did not conflict: %v", err)
	}
	if !h.workload(t).Status.Fallback.ReadyObserved {
		t.Fatal("ReadyObserved went backwards")
	}
}
