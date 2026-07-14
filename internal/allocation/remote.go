package allocation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

type RemoteAllocator struct {
	AgentClient contracts.AgentClient
	Resolver    contracts.EndpointResolver
	Now         func() time.Time
}

type preparedTarget struct {
	controlAddress string
	leaseKey       string
}

func (a *RemoteAllocator) Reserve(ctx context.Context, request contracts.ReservationRequest) (contracts.Reservation, error) {
	if a.AgentClient == nil || a.Resolver == nil {
		return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "Agent client and endpoint resolver are required")
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	expiresAt := now.Add(request.TTL)
	prepared := make([]preparedTarget, 0, len(request.Targets))
	endpoints := make([]string, 0, len(request.Targets))

	for _, target := range request.Targets {
		if target.EndpointName == "" || len(target.DeviceIDs) == 0 {
			reserveErr := contracts.NewError(contracts.ErrorKindInvalid, "reserve remote accelerator", "each target requires an endpoint name and device IDs")
			return contracts.Reservation{}, a.withRollback(ctx, prepared, reserveErr)
		}
		controlAddress, err := a.Resolver.ControlAddress(ctx, target.EndpointName)
		if err != nil {
			reserveErr := contracts.WrapError(contracts.ErrorKindTransient, "reserve remote accelerator", "resolve Agent control address", err)
			return contracts.Reservation{}, a.withRollback(ctx, prepared, reserveErr)
		}
		targetKey := targetLeaseKey(request.LeaseKey, target.EndpointName)
		reservation, err := a.AgentClient.Reserve(ctx, controlAddress, contracts.AgentReservationRequest{
			LeaseKey:    targetKey,
			DeviceIDs:   append([]string(nil), target.DeviceIDs...),
			ExpiresAt:   expiresAt,
			WorkloadUID: request.WorkloadUID,
		})
		if err != nil {
			return contracts.Reservation{}, a.withRollback(ctx, prepared, err)
		}
		if len(reservation.Endpoints) == 0 {
			prepared = append(prepared, preparedTarget{controlAddress: controlAddress, leaseKey: targetKey})
			reserveErr := contracts.NewError(contracts.ErrorKindTransient, "reserve remote accelerator", fmt.Sprintf("Agent %s returned no session endpoint", target.EndpointName))
			return contracts.Reservation{}, a.withRollback(ctx, prepared, reserveErr)
		}
		prepared = append(prepared, preparedTarget{controlAddress: controlAddress, leaseKey: targetKey})
		endpoints = append(endpoints, reservation.Endpoints...)
	}

	return contracts.Reservation{
		BindingID: "remote/" + request.LeaseKey,
		Endpoints: endpoints,
		ExpiresAt: expiresAt,
	}, nil
}

func (a *RemoteAllocator) Bind(_ context.Context, _ contracts.ReservationRequest, _ corev1.ObjectReference) error {
	return nil
}

func (a *RemoteAllocator) Release(ctx context.Context, request contracts.ReservationRequest) error {
	if a.AgentClient == nil || a.Resolver == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "release remote accelerator", "Agent client and endpoint resolver are required")
	}
	var result error
	for index := len(request.Targets) - 1; index >= 0; index-- {
		target := request.Targets[index]
		controlAddress, err := a.Resolver.ControlAddress(ctx, target.EndpointName)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		err = a.AgentClient.Release(ctx, controlAddress, targetLeaseKey(request.LeaseKey, target.EndpointName))
		result = errors.Join(result, err)
	}
	if result != nil {
		return contracts.WrapError(contracts.ErrorKindTransient, "release remote accelerator", "one or more Agent releases failed", result)
	}
	return nil
}

func (a *RemoteAllocator) rollback(ctx context.Context, prepared []preparedTarget) error {
	var result error
	for index := len(prepared) - 1; index >= 0; index-- {
		result = errors.Join(result, a.AgentClient.Release(ctx, prepared[index].controlAddress, prepared[index].leaseKey))
	}
	return result
}

func (a *RemoteAllocator) withRollback(ctx context.Context, prepared []preparedTarget, reserveErr error) error {
	if rollbackErr := a.rollback(ctx, prepared); rollbackErr != nil {
		return contracts.WrapError(
			contracts.ErrorKindTransient,
			"rollback remote accelerator",
			"reservation failed and one or more compensating releases also failed",
			errors.Join(reserveErr, rollbackErr),
		)
	}
	return reserveErr
}

func targetLeaseKey(leaseKey, endpointName string) string {
	return leaseKey + "/" + endpointName
}
