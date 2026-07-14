package allocation

import (
	"context"
	"errors"
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

type fakeResolver map[string]string

func (r fakeResolver) ControlAddress(_ context.Context, endpointName string) (string, error) {
	address, exists := r[endpointName]
	if !exists {
		return "", errors.New("not found")
	}
	return address, nil
}

type fakeAgentClient struct {
	reserveCalls  int
	releaseCalls  int
	failReserveAt int
	failRelease   bool
}

func (f *fakeAgentClient) Reserve(_ context.Context, address string, request contracts.AgentReservationRequest) (contracts.AgentReservation, error) {
	f.reserveCalls++
	if f.reserveCalls == f.failReserveAt {
		return contracts.AgentReservation{}, contracts.NewError(contracts.ErrorKindConflict, "reserve", "capacity changed")
	}
	return contracts.AgentReservation{
		BindingID: request.LeaseKey,
		Endpoints: []string{"https://" + address},
	}, nil
}

func (f *fakeAgentClient) Release(_ context.Context, _, _ string) error {
	f.releaseCalls++
	if f.failRelease {
		return errors.New("release failed")
	}
	return nil
}

func TestRemoteAllocatorRollsBackPreparedTargets(t *testing.T) {
	client := &fakeAgentClient{failReserveAt: 2}
	allocator := &RemoteAllocator{
		AgentClient: client,
		Resolver:    fakeResolver{"a": "agent-a", "b": "agent-b"},
		Now:         func() time.Time { return time.Unix(0, 0) },
	}
	_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
		LeaseKey: "lease",
		Mode:     acceleratorv1alpha1.ExecutionModeRemote,
		TTL:      time.Minute,
		Targets: []acceleratorv1alpha1.AllocationTarget{
			{EndpointName: "a", DeviceIDs: []string{"gpu-0"}},
			{EndpointName: "b", DeviceIDs: []string{"gpu-1"}},
		},
	})
	if !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
		t.Fatalf("expected capacity conflict, got %v", err)
	}
	if client.releaseCalls != 1 {
		t.Fatalf("expected one compensating release, got %d", client.releaseCalls)
	}
}

func TestRemoteAllocatorSurfacesRollbackFailureAsTransient(t *testing.T) {
	client := &fakeAgentClient{failReserveAt: 2, failRelease: true}
	allocator := &RemoteAllocator{
		AgentClient: client,
		Resolver:    fakeResolver{"a": "agent-a", "b": "agent-b"},
	}
	_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
		LeaseKey: "lease",
		TTL:      time.Minute,
		Targets: []acceleratorv1alpha1.AllocationTarget{
			{EndpointName: "a", DeviceIDs: []string{"gpu-0"}},
			{EndpointName: "b", DeviceIDs: []string{"gpu-1"}},
		},
	})
	if !contracts.IsErrorKind(err, contracts.ErrorKindTransient) {
		t.Fatalf("expected transient cleanup error, got %v", err)
	}
}
