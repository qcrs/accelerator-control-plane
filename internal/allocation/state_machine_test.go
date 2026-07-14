package allocation

import (
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAllocationTransitions(t *testing.T) {
	valid := []acceleratorv1alpha1.AllocationPhase{
		acceleratorv1alpha1.AllocationPhasePending,
		acceleratorv1alpha1.AllocationPhaseReserved,
		acceleratorv1alpha1.AllocationPhaseBound,
		acceleratorv1alpha1.AllocationPhaseReleasing,
		acceleratorv1alpha1.AllocationPhaseReleased,
	}
	lease := &acceleratorv1alpha1.AcceleratorLease{}
	for _, phase := range valid {
		if err := Transition(lease, phase); err != nil {
			t.Fatalf("transition to %s failed: %v", phase, err)
		}
	}
	if err := Transition(lease, acceleratorv1alpha1.AllocationPhaseBound); err == nil {
		t.Fatal("expected terminal transition to fail")
	}
}

func TestIsExpiredOnlyBeforeBinding(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	expires := metav1.NewTime(now.Add(-time.Second))
	lease := &acceleratorv1alpha1.AcceleratorLease{
		Status: acceleratorv1alpha1.AcceleratorLeaseStatus{
			Phase:     acceleratorv1alpha1.AllocationPhaseReserved,
			ExpiresAt: &expires,
		},
	}
	if !IsExpired(lease, now) {
		t.Fatal("expected reserved lease to expire")
	}
	lease.Status.Phase = acceleratorv1alpha1.AllocationPhaseBound
	if IsExpired(lease, now) {
		t.Fatal("bound lease must ignore reservation TTL")
	}
}
