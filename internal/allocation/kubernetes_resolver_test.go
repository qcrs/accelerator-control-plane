package allocation

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesEndpointResolverRejectsStaleConfiguredCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := acceleratorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add accelerator scheme: %v", err)
	}
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-a", Generation: 2},
		Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "remote"},
			Address: "gpu-a.example",
			Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "cricket", Version: "v1"},
			Devices: []acceleratorv1alpha1.DeviceInfo{{
				ID: "gpu-0", Health: acceleratorv1alpha1.DeviceHealthHealthy,
			}},
		},
		Status: acceleratorv1alpha1.AcceleratorEndpointStatus{Conditions: []metav1.Condition{{
			Type: "Configured", Status: metav1.ConditionTrue, ObservedGeneration: 1,
		}}},
	}
	resolver := &KubernetesEndpointResolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(endpoint).Build()}
	_, err := resolver.Resolve(context.Background(), endpoint.Name)
	if err == nil || !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
		t.Fatalf("expected stale condition conflict, got %v", err)
	}
}

func TestKubernetesEndpointResolverPublishesStaleReadyAsNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := acceleratorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add accelerator scheme: %v", err)
	}
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-a", Generation: 2},
		Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "remote"},
			Address: "gpu-a.example",
			Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "cricket", Version: "v1"},
			Devices: []acceleratorv1alpha1.DeviceInfo{{
				ID: "gpu-0", Health: acceleratorv1alpha1.DeviceHealthHealthy,
			}},
		},
		Status: acceleratorv1alpha1.AcceleratorEndpointStatus{Conditions: []metav1.Condition{
			{Type: "Configured", Status: metav1.ConditionTrue, ObservedGeneration: 2},
			{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1},
		}},
	}
	resolver := &KubernetesEndpointResolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(endpoint).Build()}
	resolved, err := resolver.Resolve(context.Background(), endpoint.Name)
	if err != nil {
		t.Fatalf("resolve endpoint: %v", err)
	}
	if resolved.Readiness == nil || resolved.Readiness.Ready || resolved.Readiness.Reason != "StaleCondition" {
		t.Fatalf("expected an explicit stale readiness failure, got %#v", resolved.Readiness)
	}
}
