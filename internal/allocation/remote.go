package allocation

import (
	"context"
	"errors"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

// RemoteAllocator materializes immutable endpoint snapshots for a reservation.
// The atomic Claim protects ownership; AcceleratorLease stores its lifecycle.
// The resolver and provider observation verify the target before materialization;
// vendor wire protocols remain inside the Provider.
// A non-transient Reserve rejection confirms no ownership remains for this
// request. Unconfirmed acquisition or rollback is always ErrorKindTransient.
type RemoteAllocator struct {
	Resolver  contracts.EndpointResolver
	Validator contracts.RemoteEndpointValidator
	Claimer   EndpointClaimer
	Now       func() time.Time
}

// EndpointClaim identifies the durable, globally unique ownership request for
// one remote runtime endpoint. Device-level sharing is intentionally outside
// the current whole-device S1/S2 scope.
type EndpointClaim struct {
	EndpointName   string
	HolderKey      string
	LeaseNamespace string
	LeaseName      string
	TTL            time.Duration
}

// EndpointClaimer turns a remote endpoint reservation into an API-server
// compare-and-swap operation. Inventory snapshots are advisory; Claim is the
// authoritative protection against stale-cache and leader-failover races.
type EndpointClaimer interface {
	Claim(ctx context.Context, claim EndpointClaim) error
	Release(ctx context.Context, claim EndpointClaim) error
}

func (a *RemoteAllocator) Reserve(ctx context.Context, request contracts.ReservationRequest) (reservation contracts.Reservation, returnErr error) {
	if a.Resolver == nil {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "endpoint resolver is required")
	}
	if a.Validator == nil {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "endpoint validator is required")
	}
	if a.Claimer == nil {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "endpoint claimer is required")
	}
	if request.Mode != acceleratorv1alpha1.ExecutionModeRemote || request.LeaseKey == "" || request.TTL <= 0 {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "remote mode, lease key, and a positive TTL are required")
	}
	if len(request.Targets) != 1 {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "exactly one exclusive endpoint target is supported")
	}

	target := request.Targets[0]
	if target.EndpointName == "" || len(target.DeviceIDs) == 0 {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "one endpoint name and device IDs are required")
	}
	if !target.Exclusive {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindUnsupported, "reserve remote accelerator", "the remote allocator supports whole-endpoint exclusive targets only")
	}
	claim := endpointClaim(request, target.EndpointName)
	if err := a.Claimer.Claim(ctx, claim); err != nil {
		return contracts.Reservation{}, err
	}
	rollback := true
	defer func() {
		if rollback {
			if releaseErr := a.Claimer.Release(ctx, claim); releaseErr != nil {
				// A failed rollback means ownership may remain. It must dominate
				// the original conflict/validation error so the controller retains
				// the pending Lease and its cleanup finalizer.
				returnErr = contracts.WrapError(contracts.ErrorKindTransient, "rollback remote reservation", "endpoint claim release is unconfirmed", errors.Join(returnErr, releaseErr))
			}
		}
	}()

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
	if endpoint.Readiness == nil {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindConflict, "reserve remote accelerator", "remote observation is missing; endpoint availability is unknown")
	}
	if !endpoint.Readiness.Ready {
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
	observer, ok := a.Validator.(contracts.EndpointObserver)
	if !ok {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindConflict, "reserve remote accelerator", "post-claim session observation is unavailable")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	observation, observeErr := observer.Observe(probeCtx, endpoint)
	cancel()
	if observeErr != nil {
		return contracts.Reservation{}, contracts.WrapError(contracts.ErrorKindTransient, "reserve remote accelerator", "post-claim runtime observation unavailable", observeErr)
	}
	observedNow := time.Now()
	if observation.Health != acceleratorv1alpha1.DeviceHealthHealthy || observation.Confidence != "Observed" || observation.ActiveSessions == nil || *observation.ActiveSessions != 0 || observation.AdvertisedCapacity != int32(len(endpoint.Devices)) || observation.ObservedAt.IsZero() || observedNow.Before(observation.ObservedAt) || observedNow.Sub(observation.ObservedAt) > contracts.ObservationMaxAge {
		sessions := "Unknown"
		if observation.ActiveSessions != nil {
			sessions = fmt.Sprint(*observation.ActiveSessions)
		}
		message := fmt.Sprintf("post-claim runtime check rejected allocation: health=%s, confidence=%s, active sessions=%s, capacity=%d (expected %d); observation must be current and session-free", observation.Health, observation.Confidence, sessions, observation.AdvertisedCapacity, len(endpoint.Devices))
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindConflict, "reserve remote accelerator", message)
	}

	if err := validateSelectedDevices(target, endpoint.Devices); err != nil {
		return contracts.Reservation{}, err
	}
	if len(target.DeviceIDs) != healthyDeviceCount(endpoint.Devices) {
		return contracts.Reservation{}, contracts.NewError(
			contracts.ErrorKindConflict,
			"reserve remote accelerator",
			fmt.Sprintf("exclusive endpoint %q exposes %d healthy devices but the workload selected %d; static runtimes cannot hide unselected devices", target.EndpointName, healthyDeviceCount(endpoint.Devices), len(target.DeviceIDs)),
		)
	}
	binding := acceleratorv1alpha1.BoundEndpoint{Name: endpoint.Name, Address: endpoint.Address, Runtime: endpoint.Runtime, Parameters: copyParameters(endpoint.Parameters)}

	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	rollback = false
	return contracts.Reservation{
		BindingID: "remote/" + request.LeaseKey,
		Endpoints: []acceleratorv1alpha1.BoundEndpoint{binding},
		ExpiresAt: now.Add(request.TTL),
	}, nil
}

func (a *RemoteAllocator) Bind(_ context.Context, _ contracts.ReservationRequest, _ corev1.ObjectReference) error {
	return nil
}

// Release accepts all recorded legacy targets, even though new reservations
// have a single target. Never shrink cleanup scope during an upgrade.
func (a *RemoteAllocator) Release(ctx context.Context, request contracts.ReservationRequest) error {
	if a.Claimer == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "release remote accelerator", "endpoint claimer is required")
	}
	seen := make(map[string]struct{}, len(request.Targets))
	claims := make([]EndpointClaim, 0, len(request.Targets))
	for _, target := range request.Targets {
		if target.EndpointName == "" {
			continue
		}
		if _, exists := seen[target.EndpointName]; exists {
			continue
		}
		seen[target.EndpointName] = struct{}{}
		claims = append(claims, endpointClaim(request, target.EndpointName))
	}
	return a.releaseClaims(ctx, claims)
}

func (a *RemoteAllocator) releaseClaims(ctx context.Context, claims []EndpointClaim) error {
	var releaseErrors []error
	for index := len(claims) - 1; index >= 0; index-- {
		if err := a.Claimer.Release(ctx, claims[index]); err != nil {
			releaseErrors = append(releaseErrors, err)
		}
	}
	return errors.Join(releaseErrors...)
}

func endpointClaim(request contracts.ReservationRequest, endpointName string) EndpointClaim {
	return EndpointClaim{
		EndpointName:   endpointName,
		HolderKey:      request.LeaseKey,
		LeaseNamespace: request.LeaseNamespace,
		LeaseName:      request.LeaseName,
		TTL:            request.TTL,
	}
}

func healthyDeviceCount(devices []acceleratorv1alpha1.DeviceInfo) int {
	count := 0
	for _, device := range devices {
		if device.Health == acceleratorv1alpha1.DeviceHealthHealthy {
			count++
		}
	}
	return count
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
