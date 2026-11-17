package mlu

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testProvider() *acceleratorv1alpha1.AcceleratorProvider {
	return &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "mlu"},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver:      DriverName,
			DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindNPU},
			ClientRuntime: &acceleratorv1alpha1.ClientRuntimeSpec{
				Image:          "mlu-remote-client:arm64",
				RuntimeVersion: "mlu-remote-test",
				SourcePath:     "/opt/mlu-remote/lib",
				MountPath:      "/opt/accelerator/mlu",
			},
		},
	}
}

func testPool() *acceleratorv1alpha1.AcceleratorPool {
	return &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "mlu-remote"},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "mlu"},
			Location:    acceleratorv1alpha1.ExecutionModeRemote,
			DeviceType:  acceleratorv1alpha1.DeviceKindNPU,
			Remote: &acceleratorv1alpha1.RemotePoolConfig{
				AllocationMode: acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint,
			},
		},
	}
}

func TestProviderBuildsRuntimePlan(t *testing.T) {
	provider := New()
	apiProvider := testProvider()
	pool := testPool()
	lease := &acceleratorv1alpha1.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1"},
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			Mode:        acceleratorv1alpha1.ExecutionModeRemote,
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "mlu"},
			PoolRef:     acceleratorv1alpha1.ClusterObjectReference{Name: "mlu-remote"},
			Request: acceleratorv1alpha1.AcceleratorRequest{
				DeviceType:    acceleratorv1alpha1.DeviceKindNPU,
				Count:         1,
				ContainerName: "worker",
			},
			Targets: []acceleratorv1alpha1.AllocationTarget{{
				EndpointName: "mlu-endpoint",
				DeviceIDs:    []string{"mlu-0"},
				Exclusive:    true,
			}},
		},
		Status: acceleratorv1alpha1.AcceleratorLeaseStatus{
			Binding: acceleratorv1alpha1.RuntimeBinding{
				ID: "remote/lease-1",
				Endpoints: []acceleratorv1alpha1.BoundEndpoint{{
					Name:    "mlu-endpoint",
					Address: "mlu-remote-server.accelerator-system.svc:50051",
					Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: DriverName, Version: "mlu-remote-test"},
				}},
			},
		},
	}

	plan, err := provider.BuildRuntimePlan(context.Background(), apiProvider, pool, lease)
	if err != nil {
		t.Fatalf("BuildRuntimePlan() error = %v", err)
	}
	if plan.Mode != acceleratorv1alpha1.ExecutionModeRemote || len(plan.InitContainers) != 1 || len(plan.Volumes) != 1 {
		t.Fatalf("unexpected runtime plan: %#v", plan)
	}
	if got := plan.Env[0].Name; got != "MLU_REMOTE_HOST" {
		t.Fatalf("first env = %q, want MLU_REMOTE_HOST", got)
	}
	if got := plan.Env[0].Value; got != "mlu-remote-server.accelerator-system.svc" {
		t.Fatalf("remote host = %q", got)
	}
	if got := plan.Env[1].Value; got != "50051" {
		t.Fatalf("remote port = %q", got)
	}
	if got := plan.Annotations[contracts.RuntimeInitContainersAnnotation]; got != runtimeInitName {
		t.Fatalf("runtime init annotation = %q", got)
	}
}

func TestProviderRejectsWrongEndpointDriver(t *testing.T) {
	endpoint := contracts.RemoteEndpoint{
		Address: "mlu-remote-server.accelerator-system.svc:50051",
		Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "other", Version: "mlu-remote-test"},
	}
	if err := New().ValidateEndpoint(endpoint); err == nil {
		t.Fatal("ValidateEndpoint() accepted a different driver")
	}
}

func TestEndpointConnectionSupportsPortParameter(t *testing.T) {
	host, port, err := endpointConnection("mlu-remote-server", map[string]string{parameterPort: "50052"})
	if err != nil {
		t.Fatalf("endpointConnection() error = %v", err)
	}
	if host != "mlu-remote-server" || port != "50052" {
		t.Fatalf("endpointConnection() = %q:%q", host, port)
	}
}
