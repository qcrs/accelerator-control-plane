package allocation

import (
	"context"
	"time"

	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

// LocalAllocator records the control-plane reservation. Concrete device IDs
// remain owned by kube-scheduler, kubelet, and the configured device plugin.
type LocalAllocator struct {
	Now func() time.Time
}

func (a *LocalAllocator) Reserve(_ context.Context, request contracts.ReservationRequest) (contracts.Reservation, error) {
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
