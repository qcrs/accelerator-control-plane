package local

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildRuntimePlanSupportsConfiguredLocalResourceForms(t *testing.T) {
	tests := []struct {
		name         string
		resourceName corev1.ResourceName
		resourceForm acceleratorv1alpha1.ResourceForm
		profile      string
		count        int32
	}{
		{name: "whole device", resourceName: "nvidia.com/gpu", resourceForm: acceleratorv1alpha1.ResourceFormWholeDevice, count: 1},
		{name: "MIG instance", resourceName: "nvidia.com/mig-1g.10gb", resourceForm: acceleratorv1alpha1.ResourceFormMIG, profile: "1g.10gb", count: 2},
		{name: "software share", resourceName: "example.com/gpu-share", resourceForm: acceleratorv1alpha1.ResourceFormShared, profile: "memory-5Gi", count: 4},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &acceleratorv1alpha1.AcceleratorProvider{Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
				Driver: DriverName, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
			}}
			pool := &acceleratorv1alpha1.AcceleratorPool{Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
				Location: acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
				Local: &acceleratorv1alpha1.LocalPoolConfig{
					ResourceName: test.resourceName, ResourceForm: test.resourceForm, Profile: test.profile,
				},
			}}
			lease := &acceleratorv1alpha1.AcceleratorLease{
				Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
					Mode: acceleratorv1alpha1.ExecutionModeLocal,
					Request: acceleratorv1alpha1.AcceleratorRequest{
						DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: test.count, ContainerName: "worker",
					},
					ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "local"},
					PoolRef:     acceleratorv1alpha1.ClusterObjectReference{Name: "pool"},
					Targets: []acceleratorv1alpha1.AllocationTarget{{
						NodeName: "node-a", Count: test.count, ResourceName: test.resourceName,
						ResourceForm: test.resourceForm, Profile: test.profile,
					}},
				},
			}

			plan, err := New().BuildRuntimePlan(context.Background(), provider, pool, lease)
			if err != nil {
				t.Fatalf("BuildRuntimePlan returned error: %v", err)
			}
			if plan.Mode != acceleratorv1alpha1.ExecutionModeLocal || plan.RequiredNodeName != "node-a" || plan.Resources == nil {
				t.Fatalf("unexpected local plan boundary: %#v", plan)
			}
			if got := plan.Resources.Requests[test.resourceName]; got.Value() != int64(test.count) {
				t.Fatalf("request for %s is %s, want %d", test.resourceName, got.String(), test.count)
			}
			if got := plan.Resources.Limits[test.resourceName]; got.Value() != int64(test.count) {
				t.Fatalf("limit for %s is %s, want %d", test.resourceName, got.String(), test.count)
			}
			if len(plan.Env) != 0 || len(plan.InitContainers) != 0 || len(plan.Volumes) != 0 || len(plan.VolumeMounts) != 0 {
				t.Fatalf("local plan contains remote runtime materialization: %#v", plan)
			}
			if plan.Annotations["accelerator.qcrs.io/resource-name"] != string(test.resourceName) ||
				plan.Annotations["accelerator.qcrs.io/resource-form"] != string(test.resourceForm) {
				t.Fatalf("resource identity was not snapshotted in annotations: %#v", plan.Annotations)
			}
		})
	}
}

func TestDifferentLocalProviderDriversReuseConfiguredResourceSemantics(t *testing.T) {
	for _, test := range []struct {
		driver   string
		resource corev1.ResourceName
	}{
		{driver: "vendor-a-device-plugin", resource: "vendor-a.example/gpu"},
		{driver: "vendor-b-share-plugin", resource: "vendor-b.example/gpu-slice"},
	} {
		implementation := NewWithDriver(test.driver)
		provider := &acceleratorv1alpha1.AcceleratorProvider{Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver: test.driver, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
		}}
		pool := &acceleratorv1alpha1.AcceleratorPool{Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			Location: acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Local: &acceleratorv1alpha1.LocalPoolConfig{ResourceName: test.resource, ResourceForm: acceleratorv1alpha1.ResourceFormCustom},
		}}
		if err := implementation.Validate(provider, pool); err != nil {
			t.Fatalf("driver %q could not validate resource %q: %v", test.driver, test.resource, err)
		}
	}
}

func TestBuildRuntimePlanRejectsRemoteLease(t *testing.T) {
	provider := &acceleratorv1alpha1.AcceleratorProvider{Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
		Driver: DriverName, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
	}}
	pool := &acceleratorv1alpha1.AcceleratorPool{Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
		Location: acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
		Local: &acceleratorv1alpha1.LocalPoolConfig{ResourceName: "nvidia.com/gpu"},
	}}
	lease := &acceleratorv1alpha1.AcceleratorLease{Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
		Mode:    acceleratorv1alpha1.ExecutionModeRemote,
		Targets: []acceleratorv1alpha1.AllocationTarget{{NodeName: "node-a", Count: 1, ResourceName: "nvidia.com/gpu"}},
	}}
	if _, err := New().BuildRuntimePlan(context.Background(), provider, pool, lease); err == nil {
		t.Fatal("Remote Lease must not produce a local RuntimePlan")
	}
}

func TestValidateRejectsNonExtendedResourceNames(t *testing.T) {
	provider := &acceleratorv1alpha1.AcceleratorProvider{Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
		Driver: DriverName, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
	}}
	for _, resourceName := range []corev1.ResourceName{
		"gpu",
		"kubernetes.io/gpu",
		"vendor.kubernetes.io/gpu",
		"bad resource/gpu",
	} {
		t.Run(string(resourceName), func(t *testing.T) {
			pool := &acceleratorv1alpha1.AcceleratorPool{Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
				Location: acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
				Local: &acceleratorv1alpha1.LocalPoolConfig{ResourceName: resourceName},
			}}
			if err := New().Validate(provider, pool); !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
				t.Fatalf("resourceName %q should be invalid, got %v", resourceName, err)
			}
		})
	}
}
