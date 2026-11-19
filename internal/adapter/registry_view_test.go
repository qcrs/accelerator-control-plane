package adapter

import (
	"testing"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

func TestProviderViewCannotRegisterWithoutAllocator(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	view := registry.Providers()
	provider := &stubProvider{driver: "unpaired", mode: api.ExecutionModeLocal}
	if err := view.Register(provider); !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("provider-only registration escaped binding authority: %v", err)
	}
	if len(view.Drivers()) != 0 || len(registry.Drivers()) != 0 {
		t.Fatal("rejected registration changed a registry")
	}
	allocator := &recordingAllocator{}
	if err := registry.Register(NewBinding(provider, allocator)); err != nil {
		t.Fatal(err)
	}
	got, err := view.Get(provider.Driver())
	if err != nil || got != provider {
		t.Fatalf("existing provider view did not reflect a new binding: %v %v", got, err)
	}
	binding, err := registry.Get(provider.Driver())
	if err != nil || binding.Provider() != got || binding.Allocator() != allocator {
		t.Fatal("provider/allocator pair diverged")
	}
}
