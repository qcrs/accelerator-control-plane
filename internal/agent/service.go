package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

type Service struct {
	Provisioner contracts.SessionProvisioner
	Now         func() time.Time
}

func (s *Service) Reserve(ctx context.Context, request contracts.AgentReservationRequest) (contracts.AgentReservation, error) {
	if s.Provisioner == nil {
		return contracts.AgentReservation{}, contracts.NewError(contracts.ErrorKindUnsupported, "prepare Agent session", "no SessionProvisioner is configured")
	}
	if request.LeaseKey == "" || len(request.DeviceIDs) == 0 {
		return contracts.AgentReservation{}, contracts.NewError(contracts.ErrorKindInvalid, "prepare Agent session", "lease key and at least one device ID are required")
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if !request.ExpiresAt.After(now) {
		return contracts.AgentReservation{}, contracts.NewError(contracts.ErrorKindInvalid, "prepare Agent session", "reservation is already expired")
	}
	seen := make(map[string]struct{}, len(request.DeviceIDs))
	for _, deviceID := range request.DeviceIDs {
		if deviceID == "" {
			return contracts.AgentReservation{}, contracts.NewError(contracts.ErrorKindInvalid, "prepare Agent session", "device ID cannot be empty")
		}
		if _, exists := seen[deviceID]; exists {
			return contracts.AgentReservation{}, contracts.NewError(contracts.ErrorKindInvalid, "prepare Agent session", fmt.Sprintf("device ID %q is duplicated", deviceID))
		}
		seen[deviceID] = struct{}{}
	}
	return s.Provisioner.Prepare(ctx, request)
}

func (s *Service) Release(ctx context.Context, leaseKey string) error {
	if s.Provisioner == nil {
		return contracts.NewError(contracts.ErrorKindUnsupported, "release Agent session", "no SessionProvisioner is configured")
	}
	if leaseKey == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "release Agent session", "lease key is required")
	}
	return s.Provisioner.Release(ctx, leaseKey)
}

type UnsupportedProvisioner struct{}

func (UnsupportedProvisioner) Prepare(_ context.Context, _ contracts.AgentReservationRequest) (contracts.AgentReservation, error) {
	return contracts.AgentReservation{}, contracts.NewError(contracts.ErrorKindUnsupported, "prepare Agent session", "Lupine process/container provisioning is not implemented")
}

func (UnsupportedProvisioner) Release(_ context.Context, _ string) error {
	return nil
}
