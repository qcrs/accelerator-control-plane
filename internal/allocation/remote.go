package allocation

import (
	"context"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

// RemoteAllocator materializes immutable endpoint snapshots for a reservation.
// Capacity ownership stays in AcceleratorLease objects; no provider-specific
// daemon or session API is required for static remote runtimes.
type RemoteAllocator struct {
	Resolver  contracts.EndpointResolver
	Validator contracts.RemoteEndpointValidator
	Now       func() time.Time
}

func (a *RemoteAllocator) Reserve(ctx context.Context, request contracts.ReservationRequest) (contracts.Reservation, error) {
	if a.Resolver == nil {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "endpoint resolver is required")
	}
	if a.Validator == nil {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "endpoint validator is required")
	}
	if request.Mode != acceleratorv1alpha1.ExecutionModeRemote || request.LeaseKey == "" || request.TTL <= 0 {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "remote mode, lease key, and a positive TTL are required")
	}
	if len(request.Targets) == 0 {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "at least one target is required")
	}

	seenEndpoints := make(map[string]struct{}, len(request.Targets))
	bindings := make([]acceleratorv1alpha1.BoundEndpoint, 0, len(request.Targets))
	for _, target := range request.Targets {
		if target.EndpointName == "" || len(target.DeviceIDs) == 0 {
			return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "each target requires an endpoint name and device IDs")
		}
		if _, exists := seenEndpoints[target.EndpointName]; exists {
			return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", fmt.Sprintf("endpoint %q is duplicated", target.EndpointName))
		}
		seenEndpoints[target.EndpointName] = struct{}{}

		endpoint, err := a.Resolver.Resolve(ctx, target.EndpointName)
		if err != nil {
			if contracts.IsErrorKind(err, contracts.ErrorKindConflict) ||
				contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable) ||
				contracts.IsErrorKind(err, contracts.ErrorKindInvalid) ||
				contracts.IsErrorKind(err, contracts.ErrorKindUnsupported) {
				return contracts.Reservation{}, err
			}
			return contracts.Reservation{}, contracts.WrapError(contracts.ErrorKindTransient, "reserve remote accelerator", "resolve endpoint", err)
		}
		if endpoint.Unschedulable {
			return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindConflict, "reserve remote accelerator", fmt.Sprintf("endpoint %q became unschedulable", target.EndpointName))
		}
		if endpoint.Readiness != nil && !endpoint.Readiness.Ready {
			message := endpoint.Readiness.Message
			if message == "" {
				message = endpoint.Readiness.Reason
			}
			if message == "" {
				message = "Ready condition is not true"
			}
			return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindConflict, "reserve remote accelerator", fmt.Sprintf("endpoint %q is explicitly not ready: %s", target.EndpointName, message))
		}
		if endpoint.Name != target.EndpointName || endpoint.PoolName != request.PoolName {
			return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindConflict, "reserve remote accelerator", fmt.Sprintf("endpoint %q no longer resolves inside pool %q", target.EndpointName, request.PoolName))
		}
		if endpoint.Address == "" {
			return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", fmt.Sprintf("endpoint %q has no runtime address", target.EndpointName))
		}
		if endpoint.Runtime.Driver == "" {
			return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", fmt.Sprintf("endpoint %q has no runtime driver", target.EndpointName))
		}
		if err := a.Validator.ValidateEndpoint(endpoint); err != nil {
			return contracts.Reservation{}, err
		}
		if err := validateSelectedDevices(target, endpoint.Devices); err != nil {
			return contracts.Reservation{}, err
		}
		bindings = append(bindings, acceleratorv1alpha1.BoundEndpoint{
			Name:       endpoint.Name,
			Address:    endpoint.Address,
			Runtime:    endpoint.Runtime,
			Parameters: copyParameters(endpoint.Parameters),
		})
	}

	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	return contracts.Reservation{
		BindingID: "remote/" + request.LeaseKey,
		Endpoints: bindings,
		ExpiresAt: now.Add(request.TTL),
	}, nil
}

func (a *RemoteAllocator) Bind(_ context.Context, _ contracts.ReservationRequest, _ corev1.ObjectReference) error {
	return nil
}

func (a *RemoteAllocator) Release(_ context.Context, _ contracts.ReservationRequest) error {
	// Removing the durable AcceleratorLease releases centrally accounted static
	// endpoint capacity. Providers with an external session API can supply a
	// different Allocator without changing the controller or Provider contract.
	return nil
}

func validateSelectedDevices(target acceleratorv1alpha1.AllocationTarget, devices []acceleratorv1alpha1.DeviceInfo) error {
	available := make(map[string]acceleratorv1alpha1.DeviceHealth, len(devices))
	for _, device := range devices {
		available[device.ID] = device.Health
	}
	seen := make(map[string]struct{}, len(target.DeviceIDs))
	for _, deviceID := range target.DeviceIDs {
		if deviceID == "" {
			return contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "selected device ID is empty")
		}
		if _, exists := seen[deviceID]; exists {
			return contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", fmt.Sprintf("device %q is duplicated", deviceID))
		}
		seen[deviceID] = struct{}{}
		health, exists := available[deviceID]
		if !exists || health != acceleratorv1alpha1.DeviceHealthHealthy {
			return contracts.NewError(contracts.ErrorKindConflict, "reserve remote accelerator", fmt.Sprintf("device %q on endpoint %q is no longer healthy and available", deviceID, target.EndpointName))
		}
	}
	return nil
}

func copyParameters(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
