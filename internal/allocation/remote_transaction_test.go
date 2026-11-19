package allocation

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

type failingRollbackClaimer struct {
	fakeEndpointClaimer
	releaseErr error
	claims     int
}

func (c *failingRollbackClaimer) Claim(ctx context.Context, claim EndpointClaim) error {
	c.claims++
	return c.fakeEndpointClaimer.Claim(ctx, claim)
}
func (c *failingRollbackClaimer) Release(ctx context.Context, claim EndpointClaim) error {
	if c.releaseErr != nil {
		return c.releaseErr
	}
	return c.fakeEndpointClaimer.Release(ctx, claim)
}

func TestRollbackFailureDominatesOriginalReservationError(t *testing.T) {
	for _, kind := range []contracts.ErrorKind{contracts.ErrorKindConflict, contracts.ErrorKindInvalid} {
		t.Run(string(kind), func(t *testing.T) {
			cause := contracts.NewError(kind, "validate test endpoint", "target changed")
			releaseErr := errors.New("API server unavailable during release")
			claimer := &failingRollbackClaimer{releaseErr: releaseErr}
			endpoint := contracts.RemoteEndpoint{Name: "a", PoolName: "remote", Address: "a", Runtime: api.RuntimeInfo{Driver: "test"}, Readiness: &contracts.EndpointReadiness{Ready: true}, Devices: []api.DeviceInfo{{ID: "0", Health: api.DeviceHealthHealthy}}}
			allocator := &RemoteAllocator{Resolver: fakeResolver{"a": endpoint}, Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return cause }), Claimer: claimer}
			request := contracts.ReservationRequest{Mode: api.ExecutionModeRemote, PoolName: "remote", LeaseKey: "held", TTL: time.Minute, Targets: []api.AllocationTarget{{EndpointName: "a", Exclusive: true, DeviceIDs: []string{"0"}}}}
			_, err := allocator.Reserve(context.Background(), request)
			if !contracts.IsErrorKind(err, contracts.ErrorKindTransient) || contracts.IsErrorKind(err, kind) {
				t.Fatalf("rollback failure classified as safe terminal rejection: %v", err)
			}
			if !errors.Is(err, cause) || !errors.Is(err, releaseErr) {
				t.Fatalf("lost acquisition/rollback cause: %v", err)
			}
			if claimer.holders["a"] != "held" {
				t.Fatal("failed release unexpectedly dropped ownership")
			}
			claimer.releaseErr = nil
			if err := allocator.Release(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if claimer.holders["a"] != "" {
				t.Fatal("confirmed release left claim held")
			}
		})
	}
}

func TestInvalidSingleTargetDoesNotAcquireClaim(t *testing.T) {
	claimer := &failingRollbackClaimer{}
	allocator := &RemoteAllocator{Resolver: fakeResolver{}, Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return nil }), Claimer: claimer}
	_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{Mode: api.ExecutionModeRemote, LeaseKey: "invalid", TTL: time.Minute, Targets: []api.AllocationTarget{{EndpointName: "a", Exclusive: true}}})
	if !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) || claimer.claims != 0 {
		t.Fatalf("invalid target touched ownership: %v claims=%d", err, claimer.claims)
	}
}

func TestReleaseStillCleansAllLegacyTargets(t *testing.T) {
	claimer := &fakeEndpointClaimer{holders: map[string]string{"a": "old", "b": "old"}}
	allocator := &RemoteAllocator{Claimer: claimer}
	if err := allocator.Release(context.Background(), contracts.ReservationRequest{LeaseKey: "old", Targets: []api.AllocationTarget{{EndpointName: "a"}, {EndpointName: "b"}, {EndpointName: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if len(claimer.holders) != 0 {
		t.Fatalf("legacy cleanup lost targets: %+v", claimer.holders)
	}
}
