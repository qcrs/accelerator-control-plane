package allocation

import (
	"context"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

// LocalAllocator records the control-plane reservation. Concrete device IDs
// remain owned by kube-scheduler, kubelet, and the configured device plugin.
type LocalAllocator struct {
	Now func() time.Time
}

func (a *LocalAllocator) Reserve(_ context.Context, request contracts.ReservationRequest) (contracts.Reservation, error) {
	if request.Mode != acceleratorv1alpha1.ExecutionModeLocal || request.LeaseKey == "" || request.TTL <= 0 {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve local accelerator", "local mode, lease key, and a positive TTL are required")
	}
	if len(request.Targets) != 1 {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve local accelerator", "exactly one local Node target is required")
	}
	target := request.Targets[0]
	if target.NodeName == "" || target.Count <= 0 || target.Count != request.Request.Count || target.ResourceName == "" || target.EndpointName != "" || len(target.DeviceIDs) != 0 {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve local accelerator", fmt.Sprintf("invalid local allocation target for node %q", target.NodeName))
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	return contracts.Reservation{
		BindingID: "local/" + request.LeaseKey,
		ExpiresAt: now.Add(request.TTL),
	}, nil
}

func (a *LocalAllocator) Bind(_ context.Context, _ contracts.ReservationRequest, _ corev1.ObjectReference) error {
	return nil
}

func (a *LocalAllocator) Release(_ context.Context, _ contracts.ReservationRequest) error {
	return nil
}
