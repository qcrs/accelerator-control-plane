package controller

import (
	"testing"
	"time"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	cricketprovider "github.com/qcrs/accelerator-control-plane/internal/provider/cricket"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestClaimLoserCanFinishFallbackWithoutReleasingWinner(t *testing.T) {
	h := newControllerHarness(t, "claim-loser", []api.ExecutionMode{api.ExecutionModeRemote, api.ExecutionModeLocal}, nil)
	w, loser := pendingReservationForTest(t, h)
	winner := &api.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{Name: "winning-lease", Namespace: w.Namespace, UID: types.UID("winning-holder")},
		Status:     api.AcceleratorLeaseStatus{Phase: api.AllocationPhaseBound},
	}
	if err := h.client.Create(h.ctx, winner); err != nil {
		t.Fatal(err)
	}
	claimer := &allocation.KubernetesEndpointClaimer{Client: h.client, APIReader: h.client, Namespace: "accelerator-system"}
	if err := claimer.Claim(h.ctx, allocation.EndpointClaim{
		EndpointName: "endpoint-a", HolderKey: string(winner.UID), LeaseNamespace: winner.Namespace,
		LeaseName: winner.Name, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	var before coordinationv1.LeaseList
	if err := h.client.List(h.ctx, &before); err != nil || len(before.Items) != 1 {
		t.Fatalf("winning Claim is missing: %v %+v", err, before.Items)
	}
	execution, err := h.adapters.Get(cricketprovider.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	execution.Allocator().(*allocation.RemoteAllocator).Claimer = claimer
	h.reconcile(t)
	w = h.workload(t)
	if len(w.Status.Fallback.History) != 1 || w.Status.Fallback.History[0].Reason != "CapacityChanged" {
		t.Fatalf("test did not reach the competing Claim failure: %+v", w.Status)
	}
	// The no-acquisition result must survive reconstruction; no in-memory
	// knowledge of the failed Reserve is available on the cleanup reconcile.
	h.reconciler = h.newReconciler()
	w = h.runUntilStarting(t, api.ExecutionModeLocal)
	if w.Status.Fallback.FailedAttempts != 1 || w.Status.AllocationAttempt != 2 {
		t.Fatalf("loser did not complete exactly one fallback: %+v", w.Status)
	}
	if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(loser), loser); err != nil {
		t.Fatal(err)
	}
	if loser.Status.Phase != api.AllocationPhaseReleased || controllerutil.ContainsFinalizer(loser, LeaseFinalizer) {
		t.Fatalf("unacquired reservation kept its cleanup finalizer: %+v", loser.Status)
	}
	var after coordinationv1.LeaseList
	if err := h.client.List(h.ctx, &after); err != nil || len(after.Items) != 1 {
		t.Fatalf("loser cleanup changed the winning Claim: %v %+v", err, after.Items)
	}
	if after.Items[0].ResourceVersion != before.Items[0].ResourceVersion ||
		after.Items[0].Spec.HolderIdentity == nil || *after.Items[0].Spec.HolderIdentity != string(winner.UID) {
		t.Fatalf("loser cleanup mutated another allocation's Claim: %+v", after.Items[0])
	}
}

func TestCleanupStillChecksUncertainAndAcquiredClaimOwnership(t *testing.T) {
	for _, uncertain := range []bool{true, false} {
		name := "acquired"
		if uncertain {
			name = "unconfirmed"
		}
		t.Run(name, func(t *testing.T) {
			var h *controllerHarness
			lease := &api.AcceleratorLease{}
			if uncertain {
				var value api.AcceleratorLease
				var failure *rollbackFailureForController
				h, value, _, failure = unconfirmedReservationForTest(t, "cleanup-uncertain")
				failure.fail = false
				*lease = value
			} else {
				h = newControllerHarness(t, "cleanup-acquired", []api.ExecutionMode{api.ExecutionModeRemote, api.ExecutionModeLocal}, nil)
				w := h.runUntilStarting(t, api.ExecutionModeRemote)
				if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Status.AllocationRef}, lease); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(lease), lease); err != nil {
				t.Fatal(err)
			}
			h.claimer.holders["endpoint-a"] = "different-holder"
			if err := h.reconciler.releaseLease(h.ctx, lease); !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
				t.Fatalf("ownership uncertainty was suppressed during cleanup: %v", err)
			}
			if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(lease), lease); err != nil {
				t.Fatal(err)
			}
			if !controllerutil.ContainsFinalizer(lease, LeaseFinalizer) || lease.Status.Phase == api.AllocationPhaseReleased || h.claimer.holders["endpoint-a"] != "different-holder" {
				t.Fatalf("cleanup completed without resolving ownership: %+v", lease.Status)
			}
		})
	}
}

func TestRejectedReservationSurvivesWorkloadStatusConflict(t *testing.T) {
	h := newControllerHarness(t, "rejected-status-conflict", []api.ExecutionMode{api.ExecutionModeRemote, api.ExecutionModeLocal}, nil)
	w, lease := pendingReservationForTest(t, h)
	// A user edit between reading the Workload and writing its failure history
	// conflicts with that status write; the Lease rejection was already saved.
	current := w.DeepCopy()
	current.Labels = map[string]string{"edited": "during-reservation"}
	if err := h.client.Update(h.ctx, current); err != nil {
		t.Fatal(err)
	}
	h.claimer.holders = map[string]string{"endpoint-a": "winning-holder"}
	_, err := h.reconciler.reserveLease(h.ctx, w, lease)
	if !apierrors.IsConflict(err) {
		t.Fatalf("test did not interrupt failure-history persistence: %v", err)
	}
	if got := h.workload(t); len(got.Status.Fallback.History) != 0 {
		t.Fatalf("failed status patch unexpectedly saved failure history: %+v", got.Status)
	}
	// Releasing the winner must not make a committed rejection get executed
	// again under the same Lease before its cleanup/fallback is recorded.
	delete(h.claimer.holders, "endpoint-a")
	h.reconciler = h.newReconciler()
	h.reconcile(t)
	w = h.workload(t)
	if len(w.Status.Fallback.History) != 1 || w.Status.Fallback.History[0].Reason != "CapacityChanged" || h.claimer.holders["endpoint-a"] != "" {
		t.Fatalf("recovery repeated a rejected reservation instead of restoring its outcome: %+v", w.Status)
	}
	w = h.runUntilStarting(t, api.ExecutionModeLocal)
	if w.Status.Fallback.FailedAttempts != 1 {
		t.Fatalf("rejected reservation consumed an incorrect fallback budget: %+v", w.Status)
	}
}
