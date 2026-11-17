package inventory

import (
	"context"
	"strings"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	localprovider "github.com/qcrs/accelerator-control-plane/internal/provider/local"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPublishedConditionsMustMatchCurrentGeneration(t *testing.T) {
	if conditionReadyOrUnknown([]metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1,
	}}, 2) {
		t.Fatal("a stale Provider or Pool Ready condition must not be trusted")
	}
	if !conditionReadyOrUnknown(nil, 2) {
		t.Fatal("an absent configuration condition remains bootstrap-compatible")
	}

	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-a", Generation: 2},
		Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
			Address: "gpu-a.example",
			Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "cricket"},
			Devices: []acceleratorv1alpha1.DeviceInfo{{
				ID: "gpu-0", Health: acceleratorv1alpha1.DeviceHealthHealthy,
			}},
		},
		Status: acceleratorv1alpha1.AcceleratorEndpointStatus{Conditions: []metav1.Condition{{
			Type: "Configured", Status: metav1.ConditionTrue, ObservedGeneration: 1,
		}}},
	}
	ready, reason := endpointReady(endpoint, "cricket")
	if ready || !strings.Contains(reason, "stale") {
		t.Fatalf("expected stale endpoint configuration to be rejected, ready=%t reason=%q", ready, reason)
	}
}

func TestKubernetesReaderNormalizesWholeMIGAndSharedLocalResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := acceleratorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add accelerator scheme: %v", err)
	}
	provider := &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver: localprovider.DriverName, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
		},
	}
	pools := []*acceleratorv1alpha1.AcceleratorPool{
		localResourcePool("whole", "nvidia.com/gpu", acceleratorv1alpha1.ResourceFormWholeDevice, "", "device", nil),
		localResourcePool("mig", "nvidia.com/mig-1g.10gb", acceleratorv1alpha1.ResourceFormMIG, "1g.10gb", "hardware", map[string]string{"slice": "mig"}),
		localResourcePool("shared", "example.com/gpu-share", acceleratorv1alpha1.ResourceFormShared, "5Gi-share", "time-sliced", map[string]string{"scheduler": "example"}),
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubernetes.io/hostname": "node-a"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				"nvidia.com/gpu":         resource.MustParse("2"),
				"nvidia.com/mig-1g.10gb": resource.MustParse("4"),
				"example.com/gpu-share":  resource.MustParse("20"),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	objects := []runtime.Object{provider, node}
	for _, pool := range pools {
		objects = append(objects, pool)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	registry, err := contracts.NewProviderRegistry(localprovider.New())
	if err != nil {
		t.Fatalf("create provider registry: %v", err)
	}
	reader := NewKubernetesReader(kubeClient, registry)
	candidates, err := reader.ListCandidates(context.Background(), contracts.SelectionRequest{
		Accelerator: acceleratorv1alpha1.AcceleratorRequest{DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: 1},
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}},
	})
	if err != nil {
		t.Fatalf("ListCandidates returned error: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("expected three local candidates, got %#v", candidates)
	}
	byPool := map[string]contracts.Candidate{}
	for _, candidate := range candidates {
		byPool[candidate.PoolName] = candidate
	}
	assertLocalCandidate(t, byPool["whole"], "nvidia.com/gpu", acceleratorv1alpha1.ResourceFormWholeDevice, "", "2")
	assertLocalCandidate(t, byPool["mig"], "nvidia.com/mig-1g.10gb", acceleratorv1alpha1.ResourceFormMIG, "1g.10gb", "4")
	assertLocalCandidate(t, byPool["shared"], "example.com/gpu-share", acceleratorv1alpha1.ResourceFormShared, "5Gi-share", "20")
	if byPool["mig"].Attributes["slice"] != "mig" || byPool["shared"].Isolation != "time-sliced" {
		t.Fatalf("local extension attributes were not normalized: mig=%#v shared=%#v", byPool["mig"], byPool["shared"])
	}
}

func localResourcePool(
	name string,
	resourceName corev1.ResourceName,
	form acceleratorv1alpha1.ResourceForm,
	profile, isolation string,
	attributes map[string]string,
) *acceleratorv1alpha1.AcceleratorPool {
	return &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "local"},
			Location:    acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Local: &acceleratorv1alpha1.LocalPoolConfig{
				ResourceName: resourceName, ResourceForm: form, Profile: profile,
				Isolation: isolation, Attributes: attributes,
			},
		},
	}
}

func assertLocalCandidate(
	t *testing.T,
	candidate contracts.Candidate,
	resourceName corev1.ResourceName,
	form acceleratorv1alpha1.ResourceForm,
	profile, available string,
) {
	t.Helper()
	if candidate.ResourceName != resourceName || candidate.ResourceForm != form || candidate.Profile != profile || !candidate.Ready {
		t.Fatalf("unexpected candidate metadata: %#v", candidate)
	}
	wanted := resource.MustParse(available)
	if candidate.AvailableQuantity.Cmp(wanted) != 0 {
		t.Fatalf("available quantity for %s is %s, want %s", resourceName, candidate.AvailableQuantity.String(), wanted.String())
	}
}
