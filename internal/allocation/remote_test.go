package allocation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

type fakeResolver map[string]contracts.RemoteEndpoint

type fakeEndpointValidator func(contracts.RemoteEndpoint) error

type fakeEndpointClaimer struct {
	holders map[string]string
}

func (c *fakeEndpointClaimer) Claim(_ context.Context, claim EndpointClaim) error {
	if c.holders == nil {
		c.holders = map[string]string{}
	}
	if holder := c.holders[claim.EndpointName]; holder != "" && holder != claim.HolderKey {
		return contracts.NewError(contracts.ErrorKindConflict, "claim test endpoint", "already claimed")
	}
	c.holders[claim.EndpointName] = claim.HolderKey
	return nil
}

func (c *fakeEndpointClaimer) Release(_ context.Context, claim EndpointClaim) error {
	if c.holders[claim.EndpointName] == claim.HolderKey {
		delete(c.holders, claim.EndpointName)
	}
	return nil
}

func (r fakeResolver) Resolve(_ context.Context, endpointName string) (contracts.RemoteEndpoint, error) {
	endpoint, exists := r[endpointName]
	if !exists {
		return contracts.RemoteEndpoint{}, errors.New("not found")
	}
	return endpoint, nil
}

func (validate fakeEndpointValidator) ValidateEndpoint(endpoint contracts.RemoteEndpoint) error {
	return validate(endpoint)
}

func TestRemoteAllocatorSnapshotsResolvedEndpoint(t *testing.T) {
	now := time.Unix(100, 0)
	allocator := &RemoteAllocator{
		Resolver: fakeResolver{"a": {
			Readiness:  &contracts.EndpointReadiness{Ready: true},
			Name:       "a",
			PoolName:   "remote",
			Address:    "gpu-a.example:43123",
			Runtime:    acceleratorv1alpha1.RuntimeInfo{Driver: "cricket"},
			Parameters: map[string]string{"rpcID": "7"},
			Devices: []acceleratorv1alpha1.DeviceInfo{{
				ID: "gpu-0", Health: acceleratorv1alpha1.DeviceHealthHealthy,
			}},
		}},
		Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return nil }),
		Claimer:   &fakeEndpointClaimer{},
		Now:       func() time.Time { return now },
	}
	reservation, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
		LeaseKey: "lease", LeaseNamespace: "default", LeaseName: "lease",
		PoolName: "remote",
		Mode:     acceleratorv1alpha1.ExecutionModeRemote,
		TTL:      time.Minute,
		Targets: []acceleratorv1alpha1.AllocationTarget{{
			EndpointName: "a", DeviceIDs: []string{"gpu-0"}, Exclusive: true,
		}},
	})
	if err != nil {
		t.Fatalf("Reserve returned error: %v", err)
	}
	if reservation.BindingID != "remote/lease" || !reservation.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("unexpected reservation metadata: %#v", reservation)
	}
	if len(reservation.Endpoints) != 1 || reservation.Endpoints[0].Address != "gpu-a.example:43123" || reservation.Endpoints[0].Runtime.Driver != "cricket" || reservation.Endpoints[0].Parameters["rpcID"] != "7" {
		t.Fatalf("unexpected endpoint snapshot: %#v", reservation.Endpoints)
	}

	reservation.Endpoints[0].Parameters["rpcID"] = "9"
	if allocator.Resolver.(fakeResolver)["a"].Parameters["rpcID"] != "7" {
		t.Fatal("reservation parameters alias resolver state")
	}
}

func TestRemoteAllocatorRejectsChangedCapacity(t *testing.T) {
	tests := []struct {
		name     string
		endpoint contracts.RemoteEndpoint
	}{
		{
			name: "unschedulable endpoint",
			endpoint: contracts.RemoteEndpoint{
				Name: "a", PoolName: "remote", Address: "gpu-a", Unschedulable: true,
				Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "cricket"},
				Devices: []acceleratorv1alpha1.DeviceInfo{{ID: "gpu-0", Health: acceleratorv1alpha1.DeviceHealthHealthy}},
			},
		},
		{
			name: "unhealthy device",
			endpoint: contracts.RemoteEndpoint{
				Name: "a", PoolName: "remote", Address: "gpu-a",
				Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "cricket"},
				Devices: []acceleratorv1alpha1.DeviceInfo{{ID: "gpu-0", Health: acceleratorv1alpha1.DeviceHealthUnhealthy}},
			},
		},
		{
			name: "explicitly unready endpoint",
			endpoint: contracts.RemoteEndpoint{
				Name: "a", PoolName: "remote", Address: "gpu-a",
				Runtime:   acceleratorv1alpha1.RuntimeInfo{Driver: "cricket"},
				Readiness: &contracts.EndpointReadiness{Ready: false, Reason: "ProbeFailed"},
				Devices:   []acceleratorv1alpha1.DeviceInfo{{ID: "gpu-0", Health: acceleratorv1alpha1.DeviceHealthHealthy}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allocator := &RemoteAllocator{
				Resolver:  fakeResolver{"a": test.endpoint},
				Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return nil }),
				Claimer:   &fakeEndpointClaimer{},
			}
			_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
				LeaseKey: "lease", LeaseNamespace: "default", LeaseName: "lease", PoolName: "remote", Mode: acceleratorv1alpha1.ExecutionModeRemote, TTL: time.Minute,
				Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "a", DeviceIDs: []string{"gpu-0"}, Exclusive: true}},
			})
			if !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
				t.Fatalf("expected capacity conflict, got %v", err)
			}
		})
	}
}

func TestRemoteAllocatorRejectsDeviceLevelTargets(t *testing.T) {
	allocator := &RemoteAllocator{
		Resolver:  fakeResolver{},
		Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return nil }),
		Claimer:   &fakeEndpointClaimer{},
	}
	_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
		LeaseKey: "lease", LeaseNamespace: "default", LeaseName: "lease",
		PoolName: "remote", Mode: acceleratorv1alpha1.ExecutionModeRemote, TTL: time.Minute,
		Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "a", DeviceIDs: []string{"gpu-0"}}},
	})
	if !contracts.IsErrorKind(err, contracts.ErrorKindUnsupported) {
		t.Fatalf("expected whole-endpoint scope rejection, got %v", err)
	}
}

func TestRemoteAllocatorRequiresExclusiveSelectionToCoverEveryVisibleDevice(t *testing.T) {
	claimer := &fakeEndpointClaimer{}
	allocator := &RemoteAllocator{
		Resolver: fakeResolver{"a": {
			Readiness: &contracts.EndpointReadiness{Ready: true},
			Name:      "a", PoolName: "remote", Address: "gpu-a",
			Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "cricket"},
			Devices: []acceleratorv1alpha1.DeviceInfo{
				{ID: "gpu-0", Health: acceleratorv1alpha1.DeviceHealthHealthy},
				{ID: "gpu-1", Health: acceleratorv1alpha1.DeviceHealthHealthy},
			},
		}},
		Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return nil }),
		Claimer:   claimer,
	}
	_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
		LeaseKey: "lease", LeaseNamespace: "default", LeaseName: "lease",
		PoolName: "remote", Mode: acceleratorv1alpha1.ExecutionModeRemote, TTL: time.Minute,
		Targets: []acceleratorv1alpha1.AllocationTarget{{
			EndpointName: "a", DeviceIDs: []string{"gpu-0"}, Exclusive: true,
		}},
	})
	if !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
		t.Fatalf("expected visibility conflict, got %v", err)
	}
	if holder := claimer.holders["a"]; holder != "" {
		t.Fatalf("failed reservation leaked endpoint claim held by %q", holder)
	}
}

func TestRemoteAllocatorUsesEndpointClaimAsFinalConflictCheck(t *testing.T) {
	claimer := &fakeEndpointClaimer{holders: map[string]string{"a": "other-lease"}}
	allocator := &RemoteAllocator{
		Resolver:  fakeResolver{},
		Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return nil }),
		Claimer:   claimer,
	}
	_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
		LeaseKey: "lease", LeaseNamespace: "default", LeaseName: "lease",
		PoolName: "remote", Mode: acceleratorv1alpha1.ExecutionModeRemote, TTL: time.Minute,
		Targets: []acceleratorv1alpha1.AllocationTarget{{
			EndpointName: "a", DeviceIDs: []string{"gpu-0"}, Exclusive: true,
		}},
	})
	if !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
		t.Fatalf("expected atomic claim conflict, got %v", err)
	}
}

func TestRemoteAllocatorRejectsAggregationBeforeClaiming(t *testing.T) {
	allocator := &RemoteAllocator{Resolver: fakeResolver{}, Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return nil }), Claimer: &fakeEndpointClaimer{}}
	_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{Mode: acceleratorv1alpha1.ExecutionModeRemote, LeaseKey: "aggregate", TTL: time.Minute, Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "a", Exclusive: true}, {EndpointName: "b", Exclusive: true}}})
	if !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("aggregation accepted: %v", err)
	}
}

func TestRemoteAllocatorRejectsUnknownAndRollsBackClaim(t *testing.T) {
	claimer := &fakeEndpointClaimer{}
	allocator := &RemoteAllocator{Resolver: fakeResolver{"a": {Name: "a", PoolName: "remote", Address: "a", Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "test"}, Devices: []acceleratorv1alpha1.DeviceInfo{{ID: "0", Health: acceleratorv1alpha1.DeviceHealthHealthy}}}}, Validator: fakeEndpointValidator(func(contracts.RemoteEndpoint) error { return nil }), Claimer: claimer}
	_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{Mode: acceleratorv1alpha1.ExecutionModeRemote, LeaseKey: "unknown", PoolName: "remote", TTL: time.Minute, Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "a", Exclusive: true, DeviceIDs: []string{"0"}}}})
	if !contracts.IsErrorKind(err, contracts.ErrorKindConflict) || claimer.holders["a"] != "" {
		t.Fatalf("unknown readiness leaked reservation: %v %+v", err, claimer.holders)
	}
}

func (validate fakeEndpointValidator) Observe(_ context.Context, endpoint contracts.RemoteEndpoint) (contracts.EndpointObservation, error) {
	sessions := int32(0)
	return contracts.EndpointObservation{Health: acceleratorv1alpha1.DeviceHealthHealthy, AdvertisedCapacity: int32(len(endpoint.Devices)), ActiveSessions: &sessions, ObservedAt: time.Now(), Confidence: "Observed"}, nil
}

type liveObservationValidator struct {
	fakeEndpointValidator
	observation contracts.EndpointObservation
	err         error
}

func (v liveObservationValidator) Observe(context.Context, contracts.RemoteEndpoint) (contracts.EndpointObservation, error) {
	return v.observation, v.err
}

func TestPostClaimProbeRejectsBusyUnhealthyAndUnknownRuntime(t *testing.T) {
	for _, state := range []string{"busy", "unhealthy", "unknown", "stale", "error"} {
		t.Run(state, func(t *testing.T) {
			sessions := int32(0)
			observation := contracts.EndpointObservation{Health: acceleratorv1alpha1.DeviceHealthHealthy, AdvertisedCapacity: 1, ActiveSessions: &sessions, Confidence: "Observed", ObservedAt: time.Now()}
			var probeErr error
			switch state {
			case "busy":
				sessions = 1
			case "unhealthy":
				observation.Health = acceleratorv1alpha1.DeviceHealthUnhealthy
			case "unknown":
				observation.ActiveSessions = nil
			case "stale":
				observation.ObservedAt = time.Now().Add(-2 * time.Minute)
			case "error":
				probeErr = fmt.Errorf("probe timeout")
			}
			claimer := &fakeEndpointClaimer{}
			allocator := &RemoteAllocator{Resolver: fakeResolver{"a": {Name: "a", PoolName: "remote", Address: "a", Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "test"}, Readiness: &contracts.EndpointReadiness{Ready: true}, Devices: []acceleratorv1alpha1.DeviceInfo{{ID: "0", Health: acceleratorv1alpha1.DeviceHealthHealthy}}}}, Validator: liveObservationValidator{fakeEndpointValidator: func(contracts.RemoteEndpoint) error { return nil }, observation: observation, err: probeErr}, Claimer: claimer}
			_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{Mode: acceleratorv1alpha1.ExecutionModeRemote, PoolName: "remote", LeaseKey: "fresh", TTL: time.Minute, Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "a", Exclusive: true, DeviceIDs: []string{"0"}}}})
			expectedKind := contracts.ErrorKindConflict
			if probeErr != nil {
				expectedKind = contracts.ErrorKindTransient
			}
			if !contracts.IsErrorKind(err, expectedKind) || claimer.holders["a"] != "" {
				t.Fatalf("stale-status window admitted %s: error=%v claims=%+v", state, err, claimer.holders)
			}
		})
	}
}
