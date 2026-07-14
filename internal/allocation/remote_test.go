package allocation

import (
	"context"
	"errors"
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

type fakeResolver map[string]contracts.RemoteEndpoint

type fakeEndpointValidator func(contracts.RemoteEndpoint) error

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
		Now:       func() time.Time { return now },
	}
	reservation, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
		LeaseKey: "lease",
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
			}
			_, err := allocator.Reserve(context.Background(), contracts.ReservationRequest{
				LeaseKey: "lease", PoolName: "remote", Mode: acceleratorv1alpha1.ExecutionModeRemote, TTL: time.Minute,
				Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "a", DeviceIDs: []string{"gpu-0"}}},
			})
			if !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
				t.Fatalf("expected capacity conflict, got %v", err)
			}
		})
	}
}
