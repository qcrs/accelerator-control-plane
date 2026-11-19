package controller

import (
	"context"
	"errors"
	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"github.com/qcrs/accelerator-control-plane/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
	"time"
)

type observedTestProvider struct {
	configurationTestProvider
	observation contracts.EndpointObservation
	err         error
}

func (p *observedTestProvider) Observe(context.Context, contracts.RemoteEndpoint) (contracts.EndpointObservation, error) {
	return p.observation, p.err
}

func TestEndpointObservationLifecycleAndRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	sessions := int32(0)
	provider := &api.AcceleratorProvider{ObjectMeta: metav1.ObjectMeta{Name: "test"}, Spec: api.AcceleratorProviderSpec{Driver: "test", DeviceKinds: []api.DeviceKind{api.DeviceKindGPU}}}
	pool := &api.AcceleratorPool{ObjectMeta: metav1.ObjectMeta{Name: "test"}, Spec: api.AcceleratorPoolSpec{ProviderRef: api.ClusterObjectReference{Name: "test"}, Location: api.ExecutionModeRemote, DeviceType: api.DeviceKindGPU, Remote: &api.RemotePoolConfig{}}}
	endpoint := &api.AcceleratorEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Generation: 1}, Spec: api.AcceleratorEndpointSpec{PoolRef: api.ClusterObjectReference{Name: "test"}, Address: "localhost:50051", Runtime: api.RuntimeInfo{Driver: "test"}, Devices: []api.DeviceInfo{{ID: "0", Health: api.DeviceHealthHealthy}}}}
	impl := &observedTestProvider{configurationTestProvider: configurationTestProvider{driver: "test", capabilities: contracts.CapabilitySet{DeviceKinds: []api.DeviceKind{api.DeviceKindGPU}, Modes: []api.ExecutionMode{api.ExecutionModeRemote}, ResourceForms: []api.ResourceForm{api.ResourceFormWholeDevice}, RemoteAllocationModes: []api.RemoteAllocationMode{api.RemoteAllocationModeExclusiveEndpoint}}}, observation: contracts.EndpointObservation{Health: api.DeviceHealthHealthy, AdvertisedCapacity: 1, ActiveSessions: &sessions, ObservedAt: now, Confidence: "Observed", Reason: "RuntimeObserved", Message: "test observation"}}
	registry, err := contracts.NewProviderRegistry(impl)
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(controllerTestScheme(t)).WithStatusSubresource(&api.AcceleratorEndpoint{}, &api.AcceleratorLease{}).WithObjects(provider, pool, endpoint).Build()
	recorder := record.NewFakeRecorder(30)
	reconciler := &EndpointReconciler{Client: kube, Providers: registry, Recorder: recorder, Now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: client.ObjectKey{Name: "endpoint"}}
	run := func() *api.AcceleratorEndpoint {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		result := &api.AcceleratorEndpoint{}
		if err := kube.Get(ctx, request.NamespacedName, result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	current := run()
	if current.Status.Available != 1 || current.Status.ActiveSessions == nil {
		t.Fatalf("idle observation: %+v", current.Status)
	}
	// Unknown is never idle, including after a previous successful observation.
	impl.err = errors.New("probe deadline exceeded")
	current = run()
	if current.Status.Available != 0 || current.Status.ActiveSessions != nil || current.Status.Health != api.DeviceHealthUnknown || current.Status.ObservedAt != nil {
		t.Fatalf("timeout retained free capacity: %+v", current.Status)
	}
	impl.err = nil
	// All consuming lease phases use the same ledger rule, and repeat/restart
	// reconciliation derives counts from objects rather than accumulating them.
	for _, phase := range []api.AllocationPhase{"", "Pending", "Reserved", "Bound", "Releasing", "Released", "Failed", "Expired"} {
		lease := &api.AcceleratorLease{ObjectMeta: metav1.ObjectMeta{Name: "lease", Namespace: "default"}, Spec: api.AcceleratorLeaseSpec{Mode: api.ExecutionModeRemote, PoolRef: api.ClusterObjectReference{Name: "test"}, Targets: []api.AllocationTarget{{EndpointName: "endpoint", Exclusive: true, DeviceIDs: []string{"0"}}}}, Status: api.AcceleratorLeaseStatus{Phase: phase}}
		if err := kube.Create(ctx, lease); err != nil {
			t.Fatal(err)
		}
		current = run()
		want := int32(0)
		if inventory.LeaseConsumesCapacity(lease) {
			want = 1
		}
		if current.Status.Reserved != want || current.Status.Capacity.Reserved != want {
			t.Fatalf("phase %s count %+v", phase, current.Status)
		}
		restarted := *reconciler
		reconciler = &restarted
		if again := run(); again.Status.Reserved != want {
			t.Fatalf("restart double counted: %+v", again.Status)
		}
		if err := kube.Delete(ctx, lease); err != nil {
			t.Fatal(err)
		}
	}
	sessions = 1
	current = run()
	condition := findMetaCondition(current.Status.Conditions, "SessionsConsistent")
	if current.Status.Available != 0 || condition.Status != metav1.ConditionFalse || condition.Reason != "UntrackedSessions" {
		t.Fatalf("untracked session not surfaced: %+v", current.Status)
	}
	if ready, _ := contracts.EndpointObservedReady(current, now); ready {
		t.Fatal("untracked session was allocatable")
	}
	foundWarning := false
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, "UntrackedSessions") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatal("untracked session warning event missing")
	}
	run()
	if len(recorder.Events) != 0 {
		t.Fatal("unchanged observation emitted repeated warning")
	}

	// Runtime failures do not delete an outstanding lease.
	lease := &api.AcceleratorLease{ObjectMeta: metav1.ObjectMeta{Name: "held", Namespace: "default"}, Spec: api.AcceleratorLeaseSpec{Mode: api.ExecutionModeRemote, Targets: []api.AllocationTarget{{EndpointName: "endpoint", Exclusive: true}}}, Status: api.AcceleratorLeaseStatus{Phase: api.AllocationPhaseBound}}
	if err := kube.Create(ctx, lease); err != nil {
		t.Fatal(err)
	}
	impl.err = errors.New("server down")
	current = run()
	if current.Status.Reserved != 1 || current.Status.Available != 0 {
		t.Fatal("probe failure released occupancy")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lease), &api.AcceleratorLease{}); err != nil {
		t.Fatal(err)
	}
	impl.err = nil
	sessions = 2
	current = run()
	if c := findMetaCondition(current.Status.Conditions, "SessionsConsistent"); c.Reason == "UntrackedSessions" {
		t.Fatal("one exclusive lease may own multiple contexts/sessions")
	}

	sessions = 0
	impl.err = nil
	current = run()
	if c := findMetaCondition(current.Status.Conditions, "SessionsConsistent"); c.Reason != "SessionUnconfirmed" || c.Status != metav1.ConditionUnknown {
		t.Fatalf("lost session not pending confirmation: %+v", c)
	}
}

// Real HTTP responses are timestamped during the probe; freshness must be
// checked after completion, rather than against the earlier reconcile time.
func TestEndpointAcceptsProbeTimestampTakenDuringRequest(t *testing.T) {
	ctx := context.Background()
	impl := &timestampTestProvider{configurationTestProvider: configurationTestProvider{driver: "clock"}}
	registry, err := contracts.NewProviderRegistry(impl)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &api.AcceleratorEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "clock", Generation: 1}, Spec: api.AcceleratorEndpointSpec{Runtime: api.RuntimeInfo{Driver: "clock"}, Devices: []api.DeviceInfo{{ID: "0", Health: api.DeviceHealthHealthy}}}}
	kube := fake.NewClientBuilder().WithScheme(controllerTestScheme(t)).Build()
	r := &EndpointReconciler{Client: kube, Providers: registry}
	if err := r.observeEndpoint(ctx, endpoint, true); err != nil {
		t.Fatal(err)
	}
	if endpoint.Status.ObservedAt == nil || endpoint.Status.Available != 1 || endpoint.Status.Health != api.DeviceHealthHealthy {
		t.Fatalf("valid timestamp rejected: %+v", endpoint.Status)
	}
}

type timestampTestProvider struct{ configurationTestProvider }

func (p *timestampTestProvider) Observe(context.Context, contracts.RemoteEndpoint) (contracts.EndpointObservation, error) {
	sessions := int32(0)
	return contracts.EndpointObservation{Health: api.DeviceHealthHealthy, AdvertisedCapacity: 1, ActiveSessions: &sessions, ObservedAt: time.Now(), Confidence: "Observed", Reason: "RuntimeObserved", Message: "current"}, nil
}
