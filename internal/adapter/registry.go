// Package adapter provides the control-plane registry for pluggable execution
// backends. It deliberately does not contain CUDA, RPC, or vendor SDK code.
package adapter

import (
	"fmt"
	"sort"
	"sync"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

// Binding connects one runtime Provider to the Allocator that owns the same
// backend's capacity. It is useful for built-in and external implementations
// alike: the controller only needs the contracts, while the application data
// plane remains inside the selected Provider's runtime plan.
type Binding struct {
	runtime  contracts.Provider
	capacity contracts.Allocator
}

func NewBinding(runtime contracts.Provider, capacity contracts.Allocator) *Binding {
	return &Binding{runtime: runtime, capacity: capacity}
}

func (b *Binding) Provider() contracts.Provider {
	if b == nil {
		return nil
	}
	return b.runtime
}

func (b *Binding) Allocator() contracts.Allocator {
	if b == nil {
		return nil
	}
	return b.capacity
}

var _ contracts.ExecutionAdapter = (*Binding)(nil)

// Registry is the single source of truth for execution adapters. Its
// ProviderRegistry view is shared with the configuration reconcilers, so a
// driver cannot be configuration-valid while its capacity adapter is absent.
type Registry struct {
	mu        sync.RWMutex
	adapters  map[string]contracts.ExecutionAdapter
	providers *contracts.ProviderRegistry
}

func NewRegistry(adapters ...contracts.ExecutionAdapter) (*Registry, error) {
	registry := &Registry{
		adapters: make(map[string]contracts.ExecutionAdapter, len(adapters)),
	}
	registry.providers = contracts.NewProviderRegistryView(registry)
	for _, execution := range adapters {
		if err := registry.Register(execution); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *Registry) Register(execution contracts.ExecutionAdapter) error {
	if r == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", "adapter registry is nil")
	}
	if execution == nil || execution.Provider() == nil || execution.Allocator() == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", "provider and allocator are required")
	}
	provider := execution.Provider()
	if provider.Driver() == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", "provider driver is required")
	}
	if err := validateCapabilities(provider.Capabilities()); err != nil {
		return err
	}
	registered := NewBinding(provider, execution.Allocator())

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[provider.Driver()]; exists {
		return contracts.NewError(contracts.ErrorKindConflict, "register execution adapter", fmt.Sprintf("driver %q is already registered", provider.Driver()))
	}
	r.adapters[provider.Driver()] = registered
	return nil
}

func validateCapabilities(capabilities contracts.CapabilitySet) error {
	if len(capabilities.DeviceKinds) == 0 || len(capabilities.Modes) == 0 || len(capabilities.ResourceForms) == 0 {
		return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", "at least one device kind, execution mode, and resource form are required")
	}
	if err := validateUniqueCapabilities("device kind", capabilities.DeviceKinds); err != nil {
		return err
	}
	if err := validateUniqueCapabilities("execution mode", capabilities.Modes); err != nil {
		return err
	}
	if err := validateUniqueCapabilities("resource form", capabilities.ResourceForms); err != nil {
		return err
	}
	if err := validateUniqueCapabilities("remote allocation mode", capabilities.RemoteAllocationModes); err != nil {
		return err
	}
	for _, kind := range capabilities.DeviceKinds {
		if kind != acceleratorv1alpha1.DeviceKindGPU && kind != acceleratorv1alpha1.DeviceKindNPU {
			return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", fmt.Sprintf("unknown device kind %q", kind))
		}
	}
	for _, form := range capabilities.ResourceForms {
		switch form {
		case acceleratorv1alpha1.ResourceFormWholeDevice,
			acceleratorv1alpha1.ResourceFormMIG,
			acceleratorv1alpha1.ResourceFormShared,
			acceleratorv1alpha1.ResourceFormCustom:
		default:
			return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", fmt.Sprintf("unknown resource form %q", form))
		}
	}
	for _, mode := range capabilities.RemoteAllocationModes {
		if mode != acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint && mode != acceleratorv1alpha1.RemoteAllocationModeDevice {
			return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", fmt.Sprintf("unknown remote allocation mode %q", mode))
		}
	}

	local := false
	remote := false
	for _, mode := range capabilities.Modes {
		switch mode {
		case acceleratorv1alpha1.ExecutionModeLocal:
			local = true
		case acceleratorv1alpha1.ExecutionModeRemote:
			remote = true
		default:
			return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", fmt.Sprintf("unknown execution mode %q", mode))
		}
	}
	if local && len(capabilities.ResourceForms) == 0 {
		return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", "a local adapter must declare supported resource forms")
	}
	if !remote && len(capabilities.RemoteAllocationModes) != 0 {
		return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", "remote allocation modes require Remote capability")
	}
	return nil
}

func validateUniqueCapabilities[T comparable](name string, values []T) error {
	seen := make(map[T]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return contracts.NewError(contracts.ErrorKindInvalid, "register execution adapter", fmt.Sprintf("%s %v is duplicated", name, value))
		}
		seen[value] = struct{}{}
	}
	return nil
}

func (r *Registry) Get(driver string) (contracts.ExecutionAdapter, error) {
	if r == nil {
		return nil, contracts.NewError(contracts.ErrorKindInvalid, "resolve execution adapter", "adapter registry is nil")
	}
	r.mu.RLock()
	execution, exists := r.adapters[driver]
	r.mu.RUnlock()
	if !exists {
		return nil, contracts.NewError(contracts.ErrorKindUnsupported, "resolve execution adapter", fmt.Sprintf("driver %q is not registered", driver))
	}
	return execution, nil
}

func (r *Registry) Drivers() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	drivers := make([]string, 0, len(r.adapters))
	for driver := range r.adapters {
		drivers = append(drivers, driver)
	}
	r.mu.RUnlock()
	sort.Strings(drivers)
	return drivers
}

// GetProvider implements the derived provider view using the same binding map.
func (r *Registry) GetProvider(driver string) (contracts.Provider, error) {
	execution, err := r.Get(driver)
	if err != nil {
		return nil, err
	}
	return execution.Provider(), nil
}

// Providers returns the provider view used by configuration and inventory
// reconciliation. The returned registry is read-only; register complete bindings
// with Register so provider and allocator views cannot diverge.
func (r *Registry) Providers() *contracts.ProviderRegistry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	providers := r.providers
	r.mu.RUnlock()
	return providers
}
