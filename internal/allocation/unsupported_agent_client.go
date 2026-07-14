package allocation

import (
	"context"
	"fmt"

	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

// UnsupportedAgentClient keeps the transport boundary explicit until an Agent
// RPC implementation is selected.
type UnsupportedAgentClient struct{}

func (UnsupportedAgentClient) Reserve(_ context.Context, controlAddress string, _ contracts.AgentReservationRequest) (contracts.AgentReservation, error) {
	return contracts.AgentReservation{}, contracts.NewError(contracts.ErrorKindUnsupported, "call Remote Agent", fmt.Sprintf("no Agent transport is configured for %q", controlAddress))
}

func (UnsupportedAgentClient) Release(_ context.Context, controlAddress, _ string) error {
	// Reserve can never succeed through this placeholder, so release is an
	// idempotent no-op and must not strand deletion finalizers.
	return nil
}
