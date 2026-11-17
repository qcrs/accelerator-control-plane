package local

import (
	"context"
	"fmt"
	"strings"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"github.com/qcrs/accelerator-control-plane/internal/injector"
	corev1 "k8s.io/api/core/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

const DriverName = "kubernetes-device-plugin"

type Provider struct {
	driver string
}

func New() *Provider { return &Provider{driver: DriverName} }

// NewWithDriver allows another Kubernetes Device Plugin integration to reuse
// the generic local runtime materialization while retaining its own Adapter
// registration and capability boundary.
func NewWithDriver(driver string) *Provider { return &Provider{driver: driver} }

func (p *Provider) Driver() string {
	if p == nil || p.driver == "" {
		return DriverName
	}
	return p.driver
}

func (p *Provider) Capabilities() contracts.CapabilitySet {
	return contracts.CapabilitySet{
		DeviceKinds: []acceleratorv1alpha1.DeviceKind{
			acceleratorv1alpha1.DeviceKindGPU,
			acceleratorv1alpha1.DeviceKindNPU,
		},
		Modes: []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeLocal},
		ResourceForms: []acceleratorv1alpha1.ResourceForm{
			acceleratorv1alpha1.ResourceFormWholeDevice,
			acceleratorv1alpha1.ResourceFormMIG,
			acceleratorv1alpha1.ResourceFormShared,
			acceleratorv1alpha1.ResourceFormCustom,
		},
	}
}

func (p *Provider) Validate(provider *acceleratorv1alpha1.AcceleratorProvider, pool *acceleratorv1alpha1.AcceleratorPool) error {
	if provider == nil || pool == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", "provider and pool are required")
	}
	if err := p.ValidateProvider(provider); err != nil {
		return err
	}
	if pool.Spec.Location != acceleratorv1alpha1.ExecutionModeLocal || pool.Spec.Local == nil || pool.Spec.Local.ResourceName == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", "a Local pool with resourceName is required")
	}
	if err := validateExtendedResourceName(pool.Spec.Local.ResourceName); err != nil {
		return err
	}
	if !supportsKind(provider.Spec.DeviceKinds, pool.Spec.DeviceType) {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate local provider", fmt.Sprintf("provider does not declare device kind %q", pool.Spec.DeviceType))
	}
	if !supportsResourceForm(p.Capabilities().ResourceForms, effectiveResourceForm(pool.Spec.Local.ResourceForm)) {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate local provider", fmt.Sprintf("resource form %q is unsupported", pool.Spec.Local.ResourceForm))
	}
	return nil
}

func (p *Provider) ValidateProvider(provider *acceleratorv1alpha1.AcceleratorProvider) error {
	if provider == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", "provider is required")
	}
	if provider.Spec.Driver != p.Driver() {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", fmt.Sprintf("driver must be %q", p.Driver()))
	}
	if provider.Spec.ClientRuntime != nil || provider.Spec.TrustBundleSecretRef != nil || provider.Spec.AuthSecretRef != nil {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate local provider", "local Device Plugin adapters do not inject a remote client runtime, TLS bundle, or authentication secret")
	}
	return nil
}

func (p *Provider) BuildRuntimePlan(
	_ context.Context,
	provider *acceleratorv1alpha1.AcceleratorProvider,
	pool *acceleratorv1alpha1.AcceleratorPool,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (contracts.RuntimePlan, error) {
	if pool == nil || lease == nil || pool.Spec.Local == nil {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build local runtime", "pool, lease, and local pool configuration are required")
	}
	if err := p.Validate(provider, pool); err != nil {
		return contracts.RuntimePlan{}, err
	}
	if lease.Spec.Mode != acceleratorv1alpha1.ExecutionModeLocal {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build local runtime", "a Local accelerator lease is required")
	}
	if len(lease.Spec.Targets) != 1 || lease.Spec.Targets[0].NodeName == "" || lease.Spec.Targets[0].Count <= 0 {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build local runtime", "local lease requires exactly one Node target with a positive count")
	}
	target := lease.Spec.Targets[0]
	resourceName := target.ResourceName
	if resourceName == "" {
		resourceName = pool.Spec.Local.ResourceName
	}
	if resourceName == "" {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build local runtime", "local lease target has no extended resource name")
	}
	resourceForm := effectiveResourceForm(target.ResourceForm)
	if target.ResourceForm == "" {
		resourceForm = effectiveResourceForm(pool.Spec.Local.ResourceForm)
	}
	profile := target.Profile
	if profile == "" {
		profile = pool.Spec.Local.Profile
	}
	resources := injector.ResourceRequirements(resourceName, target.Count)
	annotations := map[string]string{
		"accelerator.qcrs.io/mode":          string(acceleratorv1alpha1.ExecutionModeLocal),
		"accelerator.qcrs.io/provider":      lease.Spec.ProviderRef.Name,
		"accelerator.qcrs.io/pool":          lease.Spec.PoolRef.Name,
		"accelerator.qcrs.io/lease":         lease.Name,
		"accelerator.qcrs.io/resource-name": string(resourceName),
		"accelerator.qcrs.io/resource-form": string(resourceForm),
	}
	if profile != "" {
		annotations["accelerator.qcrs.io/profile"] = profile
	}
	return contracts.RuntimePlan{
		Mode:             acceleratorv1alpha1.ExecutionModeLocal,
		ContainerName:    lease.Spec.Request.ContainerName,
		Resources:        &resources,
		RequiredNodeName: target.NodeName,
		Annotations:      annotations,
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

func supportsResourceForm(forms []acceleratorv1alpha1.ResourceForm, wanted acceleratorv1alpha1.ResourceForm) bool {
	for _, form := range forms {
		if form == wanted {
			return true
		}
	}
	return false
}

func effectiveResourceForm(form acceleratorv1alpha1.ResourceForm) acceleratorv1alpha1.ResourceForm {
	if form == "" {
		return acceleratorv1alpha1.ResourceFormCustom
	}
	return form
}

func validateExtendedResourceName(name corev1.ResourceName) error {
	value := string(name)
	if problems := utilvalidation.IsQualifiedName(value); len(problems) != 0 {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", fmt.Sprintf("resourceName %q is invalid: %s", value, strings.Join(problems, "; ")))
	}
	prefix, _, qualified := strings.Cut(value, "/")
	if !qualified || prefix == "kubernetes.io" || strings.HasSuffix(prefix, ".kubernetes.io") {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate local provider", fmt.Sprintf("resourceName %q must be a vendor-qualified extended resource outside kubernetes.io", value))
	}
	return nil
}
