package controller

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	cricketprovider "github.com/qcrs/accelerator-control-plane/internal/provider/cricket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestValidateProviderConfigurationRejectsUnsupportedAuthSecret(t *testing.T) {
	provider := validCricketProviderForConfigurationTest()
	provider.Spec.AuthSecretRef = &corev1.SecretReference{Name: "cricket-auth", Namespace: "default"}
	registry, err := contracts.NewProviderRegistry(cricketprovider.New())
	if err != nil {
		t.Fatalf("create provider registry: %v", err)
	}

	reason, _, err := validateProviderConfiguration(provider, registry)
	if err == nil || reason != "UnsupportedCapability" {
		t.Fatalf("unsupported auth secret should report UnsupportedCapability, got reason=%q err=%v", reason, err)
	}
}

func TestConfigurationValidationRejectsUnsupportedResourceForms(t *testing.T) {
	t.Run("Pool", func(t *testing.T) {
		provider := &acceleratorv1alpha1.AcceleratorProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "whole-only-local"},
			Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
				Driver:      "whole-only-local",
				DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
			},
		}
		pool := &acceleratorv1alpha1.AcceleratorPool{
			ObjectMeta: metav1.ObjectMeta{Name: "shared"},
			Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
				ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: provider.Name},
				Location:    acceleratorv1alpha1.ExecutionModeLocal,
				DeviceType:  acceleratorv1alpha1.DeviceKindGPU,
				Local: &acceleratorv1alpha1.LocalPoolConfig{
					ResourceName: "example.com/gpu-share",
					ResourceForm: acceleratorv1alpha1.ResourceFormShared,
				},
			},
		}
		implementation := &configurationTestProvider{
			driver: provider.Spec.Driver,
			capabilities: contracts.CapabilitySet{
				DeviceKinds:   []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
				Modes:         []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeLocal},
				ResourceForms: []acceleratorv1alpha1.ResourceForm{acceleratorv1alpha1.ResourceFormWholeDevice},
			},
		}
		registry, err := contracts.NewProviderRegistry(implementation)
		if err != nil {
			t.Fatalf("create provider registry: %v", err)
		}
		kubeClient := fake.NewClientBuilder().WithScheme(controllerTestScheme(t)).WithObjects(provider).Build()

		reason, _, err := validatePoolConfiguration(context.Background(), kubeClient, pool, registry)
		if err == nil || reason != "UnsupportedCapability" {
			t.Fatalf("unsupported Pool resource form should report UnsupportedCapability, got reason=%q err=%v", reason, err)
		}
	})

	t.Run("Endpoint", func(t *testing.T) {
		provider := validCricketProviderForConfigurationTest()
		pool := &acceleratorv1alpha1.AcceleratorPool{
			ObjectMeta: metav1.ObjectMeta{Name: "remote"},
			Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
				ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: provider.Name},
				Location:    acceleratorv1alpha1.ExecutionModeRemote,
				DeviceType:  acceleratorv1alpha1.DeviceKindGPU,
				Remote:      &acceleratorv1alpha1.RemotePoolConfig{},
			},
		}
		endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "mig-endpoint"},
			Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
				PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: pool.Name},
				Address: "gpu.example:5000",
				Runtime: acceleratorv1alpha1.RuntimeInfo{
					Driver: cricketprovider.DriverName, Version: "test-version", APIVersion: "1",
				},
				Devices: []acceleratorv1alpha1.DeviceInfo{{
					ID: "mig-0", ResourceForm: acceleratorv1alpha1.ResourceFormMIG,
					Health: acceleratorv1alpha1.DeviceHealthHealthy,
				}},
			},
		}
		registry, err := contracts.NewProviderRegistry(cricketprovider.New())
		if err != nil {
			t.Fatalf("create provider registry: %v", err)
		}
		kubeClient := fake.NewClientBuilder().WithScheme(controllerTestScheme(t)).WithObjects(provider, pool, endpoint).Build()

		reason, _, err := validateEndpointConfiguration(context.Background(), kubeClient, endpoint, registry)
		if err == nil || reason != "UnsupportedCapability" {
			t.Fatalf("unsupported Endpoint resource form should report UnsupportedCapability, got reason=%q err=%v", reason, err)
		}
	})
}

func validCricketProviderForConfigurationTest() *acceleratorv1alpha1.AcceleratorProvider {
	return &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "cricket"},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver:      cricketprovider.DriverName,
			DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
			ClientRuntime: &acceleratorv1alpha1.ClientRuntimeSpec{
				Image: "example.test/cricket-client", RuntimeVersion: "test-version",
			},
		},
	}
}

type configurationTestProvider struct {
	driver       string
	capabilities contracts.CapabilitySet
}

func (p *configurationTestProvider) Driver() string { return p.driver }

func (p *configurationTestProvider) Capabilities() contracts.CapabilitySet {
	return p.capabilities
}

func (p *configurationTestProvider) Validate(
	*acceleratorv1alpha1.AcceleratorProvider,
	*acceleratorv1alpha1.AcceleratorPool,
) error {
	return nil
}

func (p *configurationTestProvider) BuildRuntimePlan(
	context.Context,
	*acceleratorv1alpha1.AcceleratorProvider,
	*acceleratorv1alpha1.AcceleratorPool,
	*acceleratorv1alpha1.AcceleratorLease,
) (contracts.RuntimePlan, error) {
	return contracts.RuntimePlan{}, nil
}
