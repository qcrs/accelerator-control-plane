package local

import (
	"context"
	"fmt"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"github.com/qcrs/accelerator-control-plane/internal/injector"
)

const DriverName = "kubernetes-device-plugin"

type Provider struct{}

func New() *Provider { return &Provider{} }

func (p *Provider) Driver() string { return DriverName }

func (p *Provider) Capabilities() contracts.CapabilitySet {
	return contracts.CapabilitySet{
		DeviceKinds: []acceleratorv1alpha1.DeviceKind{
			acceleratorv1alpha1.DeviceKindGPU,
			acceleratorv1alpha1.DeviceKindNPU,
		},
		Modes: []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeLocal},
	}
}

func (p *Provider) Validate(provider *acceleratorv1alpha1.AcceleratorProvider, pool *acceleratorv1alpha1.AcceleratorPool) error {
	if provider == nil || pool == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", "provider and pool are required")
	}
	if provider.Spec.Driver != DriverName {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", fmt.Sprintf("driver must be %q", DriverName))
	}
	if pool.Spec.Location != acceleratorv1alpha1.ExecutionModeLocal || pool.Spec.Local == nil || pool.Spec.Local.ResourceName == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", "a Local pool with resourceName is required")
	}
	if !supportsKind(provider.Spec.DeviceKinds, pool.Spec.DeviceType) {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate local provider", fmt.Sprintf("provider does not declare device kind %q", pool.Spec.DeviceType))
	}
	return nil
}

func (p *Provider) BuildRuntimePlan(
	_ context.Context,
	_ *acceleratorv1alpha1.AcceleratorProvider,
	pool *acceleratorv1alpha1.AcceleratorPool,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (contracts.RuntimePlan, error) {
	if pool == nil || lease == nil || pool.Spec.Local == nil {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build local runtime", "pool, lease, and local pool configuration are required")
	}
	if len(lease.Spec.Targets) != 1 || lease.Spec.Targets[0].NodeName == "" || lease.Spec.Targets[0].Count <= 0 {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build local runtime", "local lease requires exactly one Node target with a positive count")
	}
	resources := injector.ResourceRequirements(pool.Spec.Local.ResourceName, lease.Spec.Targets[0].Count)
	return contracts.RuntimePlan{
		ContainerName:    lease.Spec.Request.ContainerName,
		Resources:        &resources,
		RequiredNodeName: lease.Spec.Targets[0].NodeName,
		Annotations: map[string]string{
			"accelerator.qcrs.io/mode":     string(acceleratorv1alpha1.ExecutionModeLocal),
			"accelerator.qcrs.io/provider": lease.Spec.ProviderRef.Name,
			"accelerator.qcrs.io/pool":     lease.Spec.PoolRef.Name,
			"accelerator.qcrs.io/lease":    lease.Name,
		},
	}, nil
}

func supportsKind(kinds []acceleratorv1alpha1.DeviceKind, wanted acceleratorv1alpha1.DeviceKind) bool {
	for _, kind := range kinds {
		if kind == wanted {
			return true
		}
	}
	return false
}
