package contracts

import (
	"fmt"
	"time"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Observation freshness is a safety boundary, shared by inventory and the
// post-claim resolver. Missing/stale observations never mean free capacity.
const ObservationMaxAge = time.Minute
const ObservationInterval = 15 * time.Second

func EndpointObservationCurrent(endpoint *api.AcceleratorEndpoint, now time.Time) (bool, string) {
	for _, device := range endpoint.Spec.Devices {
		if device.Health != api.DeviceHealthHealthy {
			return false, "declared device health is not Healthy"
		}
	}
	s := endpoint.Status
	ready := meta.FindStatusCondition(s.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != endpoint.Generation {
		return false, "endpoint lacks a current Ready observation"
	}
	if s.Health != api.DeviceHealthHealthy || s.Confidence != "Observed" || s.ActiveSessions == nil || s.ObservedAt == nil {
		return false, "remote health or sessions are unknown"
	}
	age := now.Sub(s.ObservedAt.Time)
	if age < 0 || age > ObservationMaxAge {
		return false, "remote observation is stale"
	}

	return true, ""
}

func EndpointObservedReady(endpoint *api.AcceleratorEndpoint, now time.Time) (bool, string) {
	if ready, reason := EndpointObservationCurrent(endpoint, now); !ready {
		return false, reason
	}
	if *endpoint.Status.ActiveSessions != 0 {
		return false, fmt.Sprintf("runtime reports %d active sessions", *endpoint.Status.ActiveSessions)
	}
	return true, ""
}
