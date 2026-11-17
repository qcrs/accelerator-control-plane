package adapter

import (
	"context"
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

func TestRegistryKeepsProviderAndAllocatorOnOneDriverBoundary(t *testing.T) {
	localAllocator := &recordingAllocator{}
	remoteAllocator := &recordingAllocator{}
	localBinding := NewBinding(&stubProvider{driver: "local-device-plugin", mode: acceleratorv1alpha1.ExecutionModeLocal}, localAllocator)
	remoteBinding := NewBinding(&stubProvider{driver: "remote-runtime", mode: acceleratorv1alpha1.ExecutionModeRemote}, remoteAllocator)
	registry, err := NewRegistry(
		localBinding,
		remoteBinding,
	)
	if err != nil {
		t.Fatalf("create adapter registry: %v", err)
	}
	if got := registry.Drivers(); len(got) != 2 || got[0] != "local-device-plugin" || got[1] != "remote-runtime" {
		t.Fatalf("unexpected registered drivers: %#v", got)
	}
	if got := registry.Providers().Drivers(); len(got) != 2 || got[0] != "local-device-plugin" || got[1] != "remote-runtime" {
		t.Fatalf("provider view diverged from adapter registry: %#v", got)
	}
	remoteBinding.capacity = localAllocator

	execution, err := registry.Get("remote-runtime")
	if err != nil {
		t.Fatalf("resolve remote adapter: %v", err)
	}
	request := contracts.ReservationRequest{
		LeaseKey: "lease-a", Mode: acceleratorv1alpha1.ExecutionModeRemote, TTL: time.Minute,
	}
	if _, err := execution.Allocator().Reserve(context.Background(), request); err != nil {
		t.Fatalf("reserve through selected adapter: %v", err)
	}
	if remoteAllocator.reserved != 1 || localAllocator.reserved != 0 {
		t.Fatalf("reservation reached the wrong adapter: local=%d remote=%d", localAllocator.reserved, remoteAllocator.reserved)
	}
}

func TestRegistryRejectsIncompleteDuplicateAndUnknownAdapters(t *testing.T) {
	provider := &stubProvider{driver: "runtime-a", mode: acceleratorv1alpha1.ExecutionModeRemote}
	registry, err := NewRegistry(NewBinding(provider, &recordingAllocator{}))
	if err != nil {
		t.Fatalf("create adapter registry: %v", err)
	}
	if err := registry.Register(NewBinding(provider, &recordingAllocator{})); err == nil {
		t.Fatal("duplicate driver registration must fail")
	} else if !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
		t.Fatalf("duplicate driver should be a Conflict, got %v", err)
	}
	if err := registry.Register(NewBinding(nil, &recordingAllocator{})); !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("missing provider should be invalid, got %v", err)
	}
	if err := registry.Register(NewBinding(&stubProvider{driver: "runtime-b"}, nil)); !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("incomplete adapter should be invalid, got %v", err)
	}
	if _, err := registry.Get("missing"); !contracts.IsErrorKind(err, contracts.ErrorKindUnsupported) {
		t.Fatalf("unknown adapter should be unsupported, got %v", err)
	}
}

func TestRegistryRejectsInvalidCapabilityDeclarations(t *testing.T) {
	tests := []struct {
		name         string
		capabilities contracts.CapabilitySet
	}{
		{name: "empty"},
		{name: "duplicate resource form", capabilities: contracts.CapabilitySet{
			DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
			Modes:       []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeLocal},
			ResourceForms: []acceleratorv1alpha1.ResourceForm{
				acceleratorv1alpha1.ResourceFormShared, acceleratorv1alpha1.ResourceFormShared,
			},
		}},
		{name: "unknown execution mode", capabilities: contracts.CapabilitySet{
			DeviceKinds:   []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
			Modes:         []acceleratorv1alpha1.ExecutionMode{"Elsewhere"},
			ResourceForms: []acceleratorv1alpha1.ResourceForm{acceleratorv1alpha1.ResourceFormCustom},
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &stubProvider{driver: string(rune('a' + index)), capabilities: &test.capabilities}
			if _, err := NewRegistry(NewBinding(provider, &recordingAllocator{})); !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
				t.Fatalf("expected Invalid capability declaration, got %v", err)
			}
		})
	}
}

type stubProvider struct {
	driver       string
	mode         acceleratorv1alpha1.ExecutionMode
	capabilities *contracts.CapabilitySet
}

func (p *stubProvider) Driver() string { return p.driver }

func (p *stubProvider) Capabilities() contracts.CapabilitySet {
	if p.capabilities != nil {
		return *p.capabilities
	}
	return contracts.CapabilitySet{
		DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
		Modes:       []acceleratorv1alpha1.ExecutionMode{p.mode},
		ResourceForms: []acceleratorv1alpha1.ResourceForm{
			acceleratorv1alpha1.ResourceFormWholeDevice,
		},
	}
}

func (p *stubProvider) Validate(*acceleratorv1alpha1.AcceleratorProvider, *acceleratorv1alpha1.AcceleratorPool) error {
	return nil
}

func (p *stubProvider) BuildRuntimePlan(
	context.Context,
	*acceleratorv1alpha1.AcceleratorProvider,
	*acceleratorv1alpha1.AcceleratorPool,
	*acceleratorv1alpha1.AcceleratorLease,
) (contracts.RuntimePlan, error) {
	return contracts.RuntimePlan{}, nil
}

type recordingAllocator struct {
	reserved int
}

func (a *recordingAllocator) Reserve(_ context.Context, request contracts.ReservationRequest) (contracts.Reservation, error) {
	a.reserved++
	return contracts.Reservation{BindingID: request.LeaseKey, ExpiresAt: time.Now().Add(request.TTL)}, nil
}

func (a *recordingAllocator) Bind(context.Context, contracts.ReservationRequest, corev1.ObjectReference) error {
	return nil
}

func (a *recordingAllocator) Release(context.Context, contracts.ReservationRequest) error {
	return nil
}
