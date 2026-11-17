package cricket

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildRuntimePlanInjectsCricketClient(t *testing.T) {
	provider, pool := validConfiguration()
	lease := &acceleratorv1alpha1.AcceleratorLease{
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			Mode:        acceleratorv1alpha1.ExecutionModeRemote,
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "remote-cricket"},
			PoolRef:     acceleratorv1alpha1.ClusterObjectReference{Name: "remote-cricket"},
			Request:     acceleratorv1alpha1.AcceleratorRequest{ContainerName: "worker"},
			Targets: []acceleratorv1alpha1.AllocationTarget{{
				EndpointName: "gpu-a", DeviceIDs: []string{"gpu-0"}, Exclusive: true,
			}},
		},
		Status: acceleratorv1alpha1.AcceleratorLeaseStatus{
			Binding: acceleratorv1alpha1.RuntimeBinding{
				ID: "remote/lease",
				Endpoints: []acceleratorv1alpha1.BoundEndpoint{{
					Name: "gpu-a", Address: "gpu-a.example:43123",
					Runtime:    acceleratorv1alpha1.RuntimeInfo{Driver: DriverName, Version: "test-version"},
					Parameters: map[string]string{ParameterRPCID: "7"},
				}},
			},
		},
	}
	plan, err := New().BuildRuntimePlan(context.Background(), provider, pool, lease)
	if err != nil {
		t.Fatalf("BuildRuntimePlan returned error: %v", err)
	}
	environment := map[string]string{}
	for _, variable := range plan.Env {
		environment[variable.Name] = variable.Value
	}
	want := map[string]string{
		"REMOTE_GPU_ADDRESS": "gpu-a.example:43123",
		"CRICKET_RPCID":      "7",
		"LD_PRELOAD":         "/opt/accelerator/cricket/cricket-client.so",
		"LD_LIBRARY_PATH":    "/opt/accelerator/cricket",
	}
	for name, value := range want {
		if environment[name] != value {
			t.Fatalf("expected %s=%q, got %q", name, value, environment[name])
		}
	}
	if len(plan.InitContainers) != 1 || len(plan.Volumes) != 1 || len(plan.VolumeMounts) != 1 {
		t.Fatalf("runtime materialization is incomplete: %#v", plan)
	}
	if plan.Mode != acceleratorv1alpha1.ExecutionModeRemote || plan.Resources != nil || plan.RequiredNodeName != "" {
		t.Fatalf("remote plan contains a local execution request: %#v", plan)
	}
}

func TestValidateRequiresExclusiveEndpointAllocation(t *testing.T) {
	provider, pool := validConfiguration()
	pool.Spec.Remote.AllocationMode = acceleratorv1alpha1.RemoteAllocationModeDevice
	if err := New().Validate(provider, pool); err == nil {
		t.Fatal("expected Device allocation mode to be rejected")
	}
}

func TestNormalizeAddressRejectsUnsupportedForms(t *testing.T) {
	invalid := []string{
		"https://gpu.example",
		"user@gpu.example",
		"2001:db8::1",
		"gpu.example:0",
		"gpu.example:not-a-port",
	}
	for _, address := range invalid {
		t.Run(address, func(t *testing.T) {
			if _, err := normalizeAddress(address); err == nil {
				t.Fatalf("expected %q to be rejected", address)
			}
		})
	}
}

func TestValidateEndpointRequiresMatchingAPIVersion(t *testing.T) {
	endpoint := contracts.RemoteEndpoint{
		Address:    "gpu.example",
		Runtime:    acceleratorv1alpha1.RuntimeInfo{Driver: DriverName, Version: "test-version", APIVersion: "1"},
		Parameters: map[string]string{ParameterRPCID: "2"},
	}
	if err := New().ValidateEndpoint(endpoint); err == nil {
		t.Fatal("expected mismatched runtime API version and rpcID to be rejected")
	}
}

func TestValidateRejectsUnsupportedAuthentication(t *testing.T) {
	provider, _ := validConfiguration()
	provider.Spec.AuthSecretRef = &corev1.SecretReference{Name: "credentials"}
	if err := New().ValidateProvider(provider); !contracts.IsErrorKind(err, contracts.ErrorKindUnsupported) {
		t.Fatalf("expected unsupported authentication error, got %v", err)
	}
}

func validConfiguration() (*acceleratorv1alpha1.AcceleratorProvider, *acceleratorv1alpha1.AcceleratorPool) {
	return &acceleratorv1alpha1.AcceleratorProvider{
			Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
				Driver:      DriverName,
				DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
				ClientRuntime: &acceleratorv1alpha1.ClientRuntimeSpec{
					Image: "cricket-client:test", RuntimeVersion: "test-version",
				},
			},
		}, &acceleratorv1alpha1.AcceleratorPool{
			Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
				Location:   acceleratorv1alpha1.ExecutionModeRemote,
				DeviceType: acceleratorv1alpha1.DeviceKindGPU,
				Remote: &acceleratorv1alpha1.RemotePoolConfig{
					AllocationMode: acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint,
				},
			},
		}
}
