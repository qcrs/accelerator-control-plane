package inventory

import (
	"testing"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
)

func TestLeaseCapacityLifecycle(t *testing.T) {
	for _, phase := range []api.AllocationPhase{"", "Pending", "Reserved", "Bound", "Releasing", "Released", "Failed", "Expired"} {
		lease := &api.AcceleratorLease{Status: api.AcceleratorLeaseStatus{Phase: phase}}
		want := phase == "" || phase == "Pending" || phase == "Reserved" || phase == "Bound" || phase == "Releasing"
		if got := LeaseConsumesCapacity(lease); got != want {
			t.Errorf("phase %q: got %v want %v", phase, got, want)
		}
	}
}
