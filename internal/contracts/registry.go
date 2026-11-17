package contracts

import (
	"fmt"
	"sort"
	"sync"
)

type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]Provider
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
	r.mu.RLock()
	provider, exists := r.providers[driver]
	r.mu.RUnlock()
	if !exists {
		return nil, NewError(ErrorKindUnsupported, "resolve provider", fmt.Sprintf("driver %q is not registered", driver))
	}
	return provider, nil
}

func (r *ProviderRegistry) Drivers() []string {
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
