package contracts

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// ProviderSource is the read-only view of the authority that registers complete
// execution bindings. Standalone ProviderRegistry values remain useful for tests
// and configuration-only callers; adapter views never maintain a second map.
type ProviderSource interface {
	GetProvider(driver string) (Provider, error)
	Drivers() []string
}

type ProviderRegistry struct {
	source    ProviderSource
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewProviderRegistryView derives provider lookups from the binding authority.
// Registration must go through that authority so allocators cannot be omitted.
func NewProviderRegistryView(source ProviderSource) *ProviderRegistry {
	return &ProviderRegistry{source: source}
}

func NewProviderRegistry(providers ...Provider) (*ProviderRegistry, error) {
	registry := &ProviderRegistry{providers: make(map[string]Provider, len(providers))}
	for _, provider := range providers {
		if err := registry.Register(provider); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *ProviderRegistry) Register(provider Provider) error {
	if r.source != nil {
		return NewError(ErrorKindInvalid, "register provider", "provider view is read-only; register the provider and allocator together")
	}
	if provider == nil || provider.Driver() == "" {
		return NewError(ErrorKindInvalid, "register provider", "provider and driver are required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[provider.Driver()]; exists {
		return NewError(ErrorKindConflict, "register provider", fmt.Sprintf("driver %q is already registered", provider.Driver()))
	}
	r.providers[provider.Driver()] = provider
	return nil
}

func (r *ProviderRegistry) Get(driver string) (Provider, error) {
	if r.source != nil {
		return r.source.GetProvider(driver)
	}
	r.mu.RLock()
	provider, exists := r.providers[driver]
	r.mu.RUnlock()
	if !exists {
		return nil, NewError(ErrorKindUnsupported, "resolve provider", fmt.Sprintf("driver %q is not registered", driver))
	}
	return provider, nil
}

func (r *ProviderRegistry) Drivers() []string {
	if r.source != nil {
		return r.source.Drivers()
	}
	r.mu.RLock()
	drivers := make([]string, 0, len(r.providers))
	for driver := range r.providers {
		drivers = append(drivers, driver)
	}
	r.mu.RUnlock()
	sort.Strings(drivers)
	return drivers
}

// ValidateEndpoint dispatches endpoint validation to the Provider named by
// the endpoint runtime driver. Endpoint validation is optional for Providers
// that do not manage remote runtimes.
func (r *ProviderRegistry) ValidateEndpoint(endpoint RemoteEndpoint) error {
	provider, err := r.Get(endpoint.Runtime.Driver)
	if err != nil {
		return err
	}
	validator, supported := provider.(RemoteEndpointValidator)
	if !supported {
		return nil
	}
	return validator.ValidateEndpoint(endpoint)
}

// Observe dispatches through the same authoritative provider view as endpoint
// validation. It is also used after an atomic claim to close the stale-status
// window before a new remote Pod is materialized.
func (r *ProviderRegistry) Observe(ctx context.Context, endpoint RemoteEndpoint) (EndpointObservation, error) {
	provider, err := r.Get(endpoint.Runtime.Driver)
	if err != nil {
		return EndpointObservation{}, err
	}
	observer, ok := provider.(EndpointObserver)
	if !ok {
		return EndpointObservation{}, NewError(ErrorKindUnsupported, "observe endpoint", "provider has no trustworthy remote session observer")
	}
	return observer.Observe(ctx, endpoint)
}
