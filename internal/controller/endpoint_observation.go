package controller

import (
	"context"
	"fmt"
	"time"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"github.com/qcrs/accelerator-control-plane/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *EndpointReconciler) observeEndpoint(ctx context.Context, endpoint *api.AcceleratorEndpoint, configured bool) error {
	var leases api.AcceleratorLeaseList
	if err := r.List(ctx, &leases); err != nil {
		return err
	}
	reserved, bound := int32(0), int32(0)
	for _, lease := range leases.Items {
		if lease.Spec.Mode != api.ExecutionModeRemote || !inventory.LeaseConsumesCapacity(&lease) {
			continue
		}
		for _, target := range lease.Spec.Targets {
			if target.EndpointName != endpoint.Name {
				continue
			}
			reserved++
			if lease.Status.Phase == api.AllocationPhaseBound {
				bound++
			}
			break
		}
	}
	now := configNow(r.Now)
	s := &endpoint.Status
	// Reserved counts leases; Capacity.Reserved counts devices unavailable to
	// others because an exclusive lease holds the complete endpoint.
	s.Reserved = reserved
	s.Capacity = api.CapacitySummary{Total: int32(len(endpoint.Spec.Devices))}
	if reserved > 0 {
		s.Capacity.Reserved = s.Capacity.Total
	}
	s.Available, s.ActiveSessions, s.ObservedAt = 0, nil, nil
	s.Health, s.Confidence, s.ObservationSource = api.DeviceHealthUnknown, "Unknown", "lease-accounting"
	s.Reason, s.Message = "ObservationUnavailable", "remote health and sessions are unknown"
	ready := metav1.ConditionUnknown
	consistency, consistencyReason, consistencyMessage := metav1.ConditionUnknown, "SessionsUnknown", "no trustworthy runtime session observation"
	if configured {
		implementation, err := r.Providers.Get(endpoint.Spec.Runtime.Driver)
		if err != nil {
			return err
		}
		observer, ok := implementation.(contracts.EndpointObserver)
		if !ok {
			s.Reason, s.Message = "ObservationUnsupported", "provider does not support remote observation; new remote allocations are blocked"
		} else {
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			observation, err := observer.Observe(probeCtx, contracts.RemoteEndpoint{Name: endpoint.Name, PoolName: endpoint.Spec.PoolRef.Name, Address: endpoint.Spec.Address, Runtime: endpoint.Spec.Runtime, Devices: endpoint.Spec.Devices, Parameters: endpoint.Spec.Parameters})
			cancel()
			now = configNow(r.Now) // freshness is evaluated after the probe returns
			if err != nil {
				s.Reason, s.Message = "ObservationFailed", err.Error()
			} else {
				s.Reason, s.Message = observation.Reason, observation.Message
				if observation.Confidence == "Observed" && observation.ActiveSessions != nil && *observation.ActiveSessions >= 0 && !observation.ObservedAt.IsZero() && now.Sub(observation.ObservedAt) >= 0 && now.Sub(observation.ObservedAt) <= contracts.ObservationMaxAge {
					s.Health, s.Confidence, s.ObservationSource = observation.Health, "Observed", "remote-probe"
					s.ActiveSessions = observation.ActiveSessions
					observed := metav1.NewTime(observation.ObservedAt)
					s.ObservedAt = &observed
					ready = metav1.ConditionFalse
					if observation.Health == api.DeviceHealthHealthy && observation.AdvertisedCapacity == s.Capacity.Total {
						ready = metav1.ConditionTrue
					}
					for _, device := range endpoint.Spec.Devices {
						if device.Health != api.DeviceHealthHealthy {
							ready = metav1.ConditionFalse
							s.Reason, s.Message = "DeviceNotHealthy", "declared device health is not Healthy"
						}
					}
					if observation.AdvertisedCapacity != s.Capacity.Total {
						s.Health = api.DeviceHealthDegraded
						s.Reason, s.Message = "CapacityMismatch", fmt.Sprintf("server capacity %d differs from configured %d", observation.AdvertisedCapacity, s.Capacity.Total)
					}
					active := *s.ActiveSessions
					consistency, consistencyReason, consistencyMessage = metav1.ConditionTrue, "SessionsAccounted", "runtime sessions and the ledger are observationally compatible; aggregate counts cannot prove session ownership"
					if active > 0 && reserved == 0 {
						ready = metav1.ConditionFalse
						consistency, consistencyReason = metav1.ConditionFalse, "UntrackedSessions"
						consistencyMessage = fmt.Sprintf("runtime reports %d sessions with only %d outstanding leases; allocation blocked", active, reserved)
						s.Health = api.DeviceHealthDegraded
						s.Reason, s.Message = consistencyReason, consistencyMessage
					} else if bound > 0 && active == 0 {
						consistency, consistencyReason = metav1.ConditionUnknown, "SessionUnconfirmed"
						consistencyMessage = "bound lease has no observed session; startup, completion or lost session requires confirmation; reservation retained"
					}
					if ready == metav1.ConditionTrue && active == 0 && reserved == 0 {
						s.Available = s.Capacity.Total
						s.Capacity.Available = s.Available
					}
				}
			}
		}
	} else {
		ready = metav1.ConditionFalse
		s.Reason, s.Message = "ConfigurationInvalid", "endpoint configuration must be corrected before observation"
	}
	setCondition(&s.Conditions, endpoint.Generation, ConditionReady, ready, s.Reason, s.Message, now)
	setCondition(&s.Conditions, endpoint.Generation, "SessionsConsistent", consistency, consistencyReason, consistencyMessage, now)
	return nil
}
