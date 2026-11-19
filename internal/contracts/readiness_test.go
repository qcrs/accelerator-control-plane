package contracts

import (
	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
	"time"
)

func TestRemoteReadinessFailsClosed(t *testing.T) {
	now := time.Now()
	zero := int32(0)
	one := int32(1)
	at := metav1.NewTime(now)
	valid := api.AcceleratorEndpoint{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Status: api.AcceleratorEndpointStatus{Health: api.DeviceHealthHealthy, Confidence: "Observed", ObservedAt: &at, ActiveSessions: &zero, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	for _, tc := range []struct {
		name   string
		change func(*api.AcceleratorEndpoint)
	}{
		{"missing status", func(e *api.AcceleratorEndpoint) { e.Status = api.AcceleratorEndpointStatus{} }},
		{"unknown", func(e *api.AcceleratorEndpoint) { e.Status.Health = api.DeviceHealthUnknown }},
		{"unhealthy", func(e *api.AcceleratorEndpoint) { e.Status.Health = api.DeviceHealthUnhealthy }},
		{"active session", func(e *api.AcceleratorEndpoint) { e.Status.ActiveSessions = &one }},
		{"stale observation", func(e *api.AcceleratorEndpoint) {
			past := metav1.NewTime(now.Add(-2 * time.Minute))
			e.Status.ObservedAt = &past
		}},
		{"generation changed", func(e *api.AcceleratorEndpoint) { e.Generation++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := valid.DeepCopy()
			tc.change(endpoint)
			if ready, _ := EndpointObservedReady(endpoint, now); ready {
				t.Fatal("unsafe endpoint was ready")
			}
		})
	}
	if ready, reason := EndpointObservedReady(&valid, now); !ready {
		t.Fatal(reason)
	}
}
