package allocation

import (
	"context"
	"fmt"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

type Router struct {
	Local  contracts.Allocator
	Remote contracts.Allocator
}

func (r *Router) Reserve(ctx context.Context, request contracts.ReservationRequest) (contracts.Reservation, error) {
	allocator, err := r.forMode(request.Mode)
	if err != nil {
		return contracts.Reservation{}, err
	}
	return allocator.Reserve(ctx, request)
}

func (r *Router) Bind(ctx context.Context, request contracts.ReservationRequest, pod corev1.ObjectReference) error {
	allocator, err := r.forMode(request.Mode)
	if err != nil {
		return err
	}
	return allocator.Bind(ctx, request, pod)
}

func (r *Router) Release(ctx context.Context, request contracts.ReservationRequest) error {
	allocator, err := r.forMode(request.Mode)
	if err != nil {
		return err
	}
	return allocator.Release(ctx, request)
}

func (r *Router) forMode(mode acceleratorv1alpha1.ExecutionMode) (contracts.Allocator, error) {
	var allocator contracts.Allocator
	switch mode {
	case acceleratorv1alpha1.ExecutionModeLocal:
		allocator = r.Local
	case acceleratorv1alpha1.ExecutionModeRemote:
		allocator = r.Remote
	default:
		return nil, contracts.NewError(contracts.ErrorKindUnsupported, "route allocator", fmt.Sprintf("execution mode %q is unsupported", mode))
	}
	if allocator == nil {
		return nil, contracts.NewError(contracts.ErrorKindUnsupported, "route allocator", fmt.Sprintf("execution mode %q has no allocator", mode))
	}
	return allocator, nil
}
