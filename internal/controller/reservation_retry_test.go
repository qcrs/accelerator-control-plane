package controller

import (
	"context"
	"errors"
	"testing"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	cricketprovider "github.com/qcrs/accelerator-control-plane/internal/provider/cricket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type rollbackFailureForController struct {
	allocation.EndpointClaimer
	fail bool
}

func (c *rollbackFailureForController) Release(ctx context.Context, claim allocation.EndpointClaim) error {
	if c.fail {
		return errors.New("temporary claim delete failure")
	}
	return c.EndpointClaimer.Release(ctx, claim)
}

type changedAfterClaimResolver struct {
	contracts.EndpointResolver
	unavailable bool
}

func (r *changedAfterClaimResolver) Resolve(ctx context.Context, name string) (contracts.RemoteEndpoint, error) {
	endpoint, err := r.EndpointResolver.Resolve(ctx, name)
	if err == nil && r.unavailable {
		endpoint.Readiness = &contracts.EndpointReadiness{Ready: false, Reason: "ChangedAfterSelection", Message: "endpoint health changed after selection"}
	}
	return endpoint, err
}

func unconfirmedReservationForTest(t *testing.T, name string) (*controllerHarness, api.AcceleratorLease, *changedAfterClaimResolver, *rollbackFailureForController) {
	t.Helper()
	h := newControllerHarness(t, name, []api.ExecutionMode{api.ExecutionModeRemote, api.ExecutionModeLocal}, nil)
	execution, err := h.adapters.Get(cricketprovider.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	allocator := execution.Allocator().(*allocation.RemoteAllocator)
	resolver := &changedAfterClaimResolver{EndpointResolver: allocator.Resolver, unavailable: true}
	claimer := &rollbackFailureForController{EndpointClaimer: allocator.Claimer, fail: true}
	allocator.Resolver = resolver
	allocator.Claimer = claimer
	var lease api.AcceleratorLease
	for i := 0; i < 8; i++ {
		h.reconcile(t)
		w := h.workload(t)
		if w.Status.AllocationRef == "" {
			continue
		}
		if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Status.AllocationRef}, &lease); err != nil {
			t.Fatal(err)
		}
		if lease.Status.Phase == api.AllocationPhasePending {
			break
		}
	}
	if lease.Status.Phase != api.AllocationPhasePending {
		t.Fatal("did not reach Pending lease")
	}
	_, err = h.reconciler.Reconcile(h.ctx, h.request)
	if !contracts.IsErrorKind(err, contracts.ErrorKindTransient) {
		t.Fatalf("rollback uncertainty was not retried: %v", err)
	}
	return h, lease, resolver, claimer
}

func TestFailedReservationRollbackRetainsLeaseAndDoesNotFallback(t *testing.T) {
	h, lease, resolver, claimer := unconfirmedReservationForTest(t, "rollback-owned")
	ref := lease.Name
	w := h.workload(t)
	if w.Status.AllocationRef != ref || w.Status.Fallback.FailedAttempts != 0 || len(w.Status.Fallback.History) != 0 || w.Status.PodRef != nil {
		t.Fatalf("uncertain ownership caused a new attempt or Pod: %+v", w.Status)
	}
	if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(&lease), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.Status.Phase != api.AllocationPhasePending || !controllerutil.ContainsFinalizer(&lease, LeaseFinalizer) {
		t.Fatalf("lease marked terminal despite unconfirmed release: %+v", lease)
	}
	if c := findMetaCondition(w.Status.Conditions, ConditionReserved); c == nil || c.Reason != "ReservationRetrying" {
		t.Fatalf("retry reason is not visible: %+v", c)
	}
	if h.claimer.holders["endpoint-a"] == "" {
		t.Fatal("test did not retain a held claim")
	}
	// Reconstruct the controller and retry the same lease after recovery.
	resolver.unavailable = false
	claimer.fail = false
	h.reconciler = h.newReconciler()
	w = h.runUntilStarting(t, api.ExecutionModeRemote)
	if w.Status.AllocationRef != ref || w.Status.Fallback.FailedAttempts != 0 {
		t.Fatalf("recovery used a second allocation: %+v", w.Status)
	}
	var leases api.AcceleratorLeaseList
	if err := h.client.List(h.ctx, &leases); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) != 1 {
		t.Fatalf("recovery created duplicate leases: %d", len(leases.Items))
	}
}

func TestConfigurationDeletionAfterUnconfirmedReservationStillCleansClaim(t *testing.T) {
	for _, kind := range []string{"provider", "pool"} {
		t.Run(kind, func(t *testing.T) {
			h, lease, _, claimer := unconfirmedReservationForTest(t, "rollback-deleted-"+kind)
			var object client.Object = &api.AcceleratorProvider{ObjectMeta: metav1.ObjectMeta{Name: lease.Spec.ProviderRef.Name}}
			if kind == "pool" {
				object = &api.AcceleratorPool{ObjectMeta: metav1.ObjectMeta{Name: lease.Spec.PoolRef.Name}}
			}
			if err := h.client.Delete(h.ctx, object); err != nil {
				t.Fatal(err)
			}
			// Failed cleanup in an earlier reconcile can leave a Claim even
			// though this reconcile cannot resolve the current configuration.
			_, err := h.reconciler.Reconcile(h.ctx, h.request)
			if err == nil {
				t.Fatal("configuration deletion bypassed the unconfirmed ownership cleanup")
			}
			if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(&lease), &lease); err != nil {
				t.Fatal(err)
			}
			w := h.workload(t)
			if lease.Status.Phase != api.AllocationPhasePending || !controllerutil.ContainsFinalizer(&lease, LeaseFinalizer) ||
				w.Status.AllocationRef != lease.Name || w.Status.PodRef != nil || h.claimer.holders["endpoint-a"] == "" {
				t.Fatal("configuration loss discarded a still-owned reservation")
			}
			// Cleanup uses the persisted driver, independent of live Pool or
			// Provider configuration. It must also survive a controller restart.
			claimer.fail = false
			h.reconciler = h.newReconciler()
			h.reconcile(t)
			if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(&lease), &lease); err != nil {
				t.Fatal(err)
			}
			if h.claimer.holders["endpoint-a"] != "" || controllerutil.ContainsFinalizer(&lease, LeaseFinalizer) ||
				lease.Status.Phase != api.AllocationPhaseFailed {
				t.Fatal("persisted driver did not clean the old claim before ending the allocation")
			}
		})
	}
}
