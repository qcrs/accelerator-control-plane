package agent

import (
	"context"
	"testing"
	"time"

	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

type fakeProvisioner struct {
	prepared int
	released int
}

func (f *fakeProvisioner) Prepare(_ context.Context, _ contracts.AgentReservationRequest) (contracts.AgentReservation, error) {
	f.prepared++
	return contracts.AgentReservation{BindingID: "binding", Endpoints: []string{"https://gpu.example"}}, nil
}

func (f *fakeProvisioner) Release(_ context.Context, _ string) error {
	f.released++
	return nil
}

func TestServiceValidatesAndDelegates(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	provisioner := &fakeProvisioner{}
	service := &Service{Provisioner: provisioner, Now: func() time.Time { return now }}
	reservation, err := service.Reserve(context.Background(), contracts.AgentReservationRequest{
		LeaseKey:  "lease/endpoint",
		DeviceIDs: []string{"gpu-0"},
		ExpiresAt: now.Add(time.Minute),
	})
	if err != nil || reservation.BindingID != "binding" || provisioner.prepared != 1 {
		t.Fatalf("unexpected reservation: %#v, %v", reservation, err)
	}
	if err := service.Release(context.Background(), "lease/endpoint"); err != nil || provisioner.released != 1 {
		t.Fatalf("release failed: %v", err)
	}
}

func TestServiceRejectsExpiredOrDuplicateDevices(t *testing.T) {
	now := time.Now()
	service := &Service{Provisioner: &fakeProvisioner{}, Now: func() time.Time { return now }}
	requests := []contracts.AgentReservationRequest{
		{LeaseKey: "lease", DeviceIDs: []string{"gpu-0"}, ExpiresAt: now},
		{LeaseKey: "lease", DeviceIDs: []string{"gpu-0", "gpu-0"}, ExpiresAt: now.Add(time.Minute)},
	}
	for _, request := range requests {
		if _, err := service.Reserve(context.Background(), request); !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
			t.Fatalf("expected invalid request error, got %v", err)
		}
	}
}
