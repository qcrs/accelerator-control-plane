package allocation

import (
	"context"
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

func TestLocalAllocatorIsIdempotentForLeaseKey(t *testing.T) {
	allocator := &LocalAllocator{Now: func() time.Time { return time.Unix(100, 0) }}
	request := contracts.ReservationRequest{
		LeaseKey: "lease-a", Mode: acceleratorv1alpha1.ExecutionModeLocal, TTL: time.Minute,
		Request: acceleratorv1alpha1.AcceleratorRequest{DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: 2},
		Targets: []acceleratorv1alpha1.AllocationTarget{{
			NodeName: "node-a", Count: 2, ResourceName: "example.com/gpu-share",
			ResourceForm: acceleratorv1alpha1.ResourceFormShared,
		}},
	}
	first, err := allocator.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("first Reserve returned error: %v", err)
	}
	second, err := allocator.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("second Reserve returned error: %v", err)
	}
	if first.BindingID != second.BindingID || !first.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatalf("idempotent Reserve changed result: first=%#v second=%#v", first, second)
	}
	if err := allocator.Bind(context.Background(), request, corev1.ObjectReference{Name: "pod"}); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	if err := allocator.Release(context.Background(), request); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if err := allocator.Release(context.Background(), request); err != nil {
		t.Fatalf("idempotent Release returned error: %v", err)
	}
}

func TestLocalAllocatorRejectsMismatchedRequestAndTargetUnits(t *testing.T) {
	_, err := (&LocalAllocator{}).Reserve(context.Background(), contracts.ReservationRequest{
		LeaseKey: "lease-a", Mode: acceleratorv1alpha1.ExecutionModeLocal, TTL: time.Minute,
		Request: acceleratorv1alpha1.AcceleratorRequest{Count: 2},
		Targets: []acceleratorv1alpha1.AllocationTarget{{
			NodeName: "node-a", Count: 1, ResourceName: "nvidia.com/gpu",
		}},
	})
	if !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("expected mismatched units to be invalid, got %v", err)
	}
}
