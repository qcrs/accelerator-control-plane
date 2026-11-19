package corex

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func providerFixture() *acceleratorv1alpha1.AcceleratorProvider {
	return &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "corex"},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver:      DriverName,
			DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
			ClientRuntime: &acceleratorv1alpha1.ClientRuntimeSpec{
				Image:          "corex-remote-client:arm64",
				RuntimeVersion: "corex-remote-arm64-4.4",
				SourcePath:     "/opt/corex-remote/lib",
				MountPath:      "/opt/accelerator/corex",
			},
		},
	}
}

func poolFixture() *acceleratorv1alpha1.AcceleratorPool {
	return &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "corex-remote"},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "corex"},
			Location:    acceleratorv1alpha1.ExecutionModeRemote,
			DeviceType:  acceleratorv1alpha1.DeviceKindGPU,
			Remote:      &acceleratorv1alpha1.RemotePoolConfig{AllocationMode: acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint},
		},
	}
}

func TestProviderBuildsRuntimePlan(t *testing.T) {
	lease := &acceleratorv1alpha1.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1"},
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			Mode:        acceleratorv1alpha1.ExecutionModeRemote,
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "corex"},
			PoolRef:     acceleratorv1alpha1.ClusterObjectReference{Name: "corex-remote"},
			Request: acceleratorv1alpha1.AcceleratorRequest{
				DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: 1, ContainerName: "worker",
			},
			Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "corex-server", DeviceIDs: []string{"gpu-0"}, Exclusive: true}},
		},
		Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Binding: acceleratorv1alpha1.RuntimeBinding{
			ID: "remote/lease-1",
			Endpoints: []acceleratorv1alpha1.BoundEndpoint{{
				Name: "corex-server", Address: "corex-server.accelerator-system.svc:50051",
				Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: DriverName, Version: "corex-remote-arm64-4.4"},
			}},
		}},
	}
	plan, err := New().BuildRuntimePlan(context.Background(), providerFixture(), poolFixture(), lease)
	if err != nil {
		t.Fatalf("BuildRuntimePlan() error = %v", err)
	}
	if plan.Mode != acceleratorv1alpha1.ExecutionModeRemote || len(plan.InitContainers) != 1 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	values := map[string]string{}
	for _, env := range plan.Env {
		values[env.Name] = env.Value
	}
	if values["COREX_REMOTE_HOST"] != "corex-server.accelerator-system.svc" || values["COREX_REMOTE_PORT"] != "50051" {
		t.Fatalf("unexpected endpoint env: %#v", values)
	}
	if _, found := values["LD_PRELOAD"]; found {
		t.Fatalf("CoreX must not inject LD_PRELOAD: %q", values["LD_PRELOAD"])
	}
	if plan.Annotations[contracts.RuntimeInitContainersAnnotation] != runtimeInitName {
		t.Fatalf("missing runtime init annotation: %#v", plan.Annotations)
	}
}

func TestEndpointConnectionUsesPortParameter(t *testing.T) {
	host, port, err := endpointConnection("corex-server", map[string]string{parameterPort: "50052"})
	if err != nil {
		t.Fatalf("endpointConnection() error = %v", err)
	}
	if host != "corex-server" || port != "50052" {
		t.Fatalf("endpointConnection() = %q:%q", host, port)
	}
}

// A nonexistent Kubernetes DNS name must be passed through unchanged, without
// doing network resolution in the control-plane process.
func TestRuntimePlanPreservesStatefulDNS(t *testing.T) {
	lease := &acceleratorv1alpha1.AcceleratorLease{
		Spec:   acceleratorv1alpha1.AcceleratorLeaseSpec{Mode: acceleratorv1alpha1.ExecutionModeRemote, Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "server", Exclusive: true}}},
		Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Binding: acceleratorv1alpha1.RuntimeBinding{ID: "binding", Endpoints: []acceleratorv1alpha1.BoundEndpoint{{Name: "server", Address: "server-0.server.test.svc.cluster.local:50051", Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: DriverName, Version: "corex-remote-arm64-4.4"}}}}},
	}
	plan, err := New().BuildRuntimePlan(context.Background(), providerFixture(), poolFixture(), lease)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range plan.Env {
		if v.Name == "COREX_REMOTE_HOST" {
			if v.Value != "server-0.server.test.svc.cluster.local" {
				t.Fatal(v.Value)
			}
			return
		}
	}
	t.Fatal("missing host")
}
