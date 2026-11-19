package inventory

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSummarizeLocalPoolCapacityIncludesPodsAndUnboundLeases(t *testing.T) {
	scheme := capacityTestScheme(t)
	resourceName := corev1.ResourceName("nvidia.com/gpu")
	pool := &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "local"},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			Location: acceleratorv1alpha1.ExecutionModeLocal,
			Local:    &acceleratorv1alpha1.LocalPoolConfig{ResourceName: resourceName},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{resourceName: resource.MustParse("4")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "default", UID: types.UID("pod-uid")},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{resourceName: resource.MustParse("1")},
			}}},
		},
	}
	lease := &acceleratorv1alpha1.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{Name: "reserved", Namespace: "default"},
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: pool.Name},
			Mode:    acceleratorv1alpha1.ExecutionModeLocal,
			Targets: []acceleratorv1alpha1.AllocationTarget{{NodeName: node.Name, Count: 2}},
		},
		Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseReserved},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, node, pod, lease).Build()
	summary, readyEndpoints, err := SummarizePoolCapacity(context.Background(), reader, pool, "local", nil)
	if err != nil {
		t.Fatalf("summarize local capacity: %v", err)
	}
	if summary.Total != 4 || summary.Available != 1 || summary.Reserved != 3 || readyEndpoints != 0 {
		t.Fatalf("unexpected local summary: %#v, readyEndpoints=%d", summary, readyEndpoints)
	}
}

func TestSummarizeRemotePoolCapacityTracksExclusiveLease(t *testing.T) {
	scheme := capacityTestScheme(t)
	pool := &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "remote"},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			Location: acceleratorv1alpha1.ExecutionModeRemote,
			Remote:   &acceleratorv1alpha1.RemotePoolConfig{AllocationMode: acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint},
		},
	}
	endpointA := capacityEndpoint("endpoint-a", pool.Name, 2)
	endpointB := capacityEndpoint("endpoint-b", pool.Name, 1)
	lease := &acceleratorv1alpha1.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{Name: "reserved", Namespace: "default"},
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: pool.Name},
			Mode:    acceleratorv1alpha1.ExecutionModeRemote,
			Targets: []acceleratorv1alpha1.AllocationTarget{{
				EndpointName: endpointA.Name, DeviceIDs: []string{"gpu-0", "gpu-1"}, Exclusive: true, Count: 2,
			}},
		},
		Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseReserved},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, endpointA, endpointB, lease).Build()
	summary, readyEndpoints, err := SummarizePoolCapacity(context.Background(), reader, pool, "cricket", nil)
	if err != nil {
		t.Fatalf("summarize remote capacity: %v", err)
	}
	if summary.Total != 3 || summary.Available != 1 || summary.Reserved != 2 || readyEndpoints != 2 {
		t.Fatalf("unexpected remote summary: %#v, readyEndpoints=%d", summary, readyEndpoints)
	}
}

func capacityTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := acceleratorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add accelerator scheme: %v", err)
	}
	return scheme
}

func capacityEndpoint(name, poolName string, count int) *acceleratorv1alpha1.AcceleratorEndpoint {
	devices := make([]acceleratorv1alpha1.DeviceInfo, 0, count)
	for index := 0; index < count; index++ {
		devices = append(devices, acceleratorv1alpha1.DeviceInfo{
			ID: string(rune('0' + index)), Health: acceleratorv1alpha1.DeviceHealthHealthy,
		})
	}
	// Keep human-readable IDs for the Lease fixture above.
	for index := range devices {
		devices[index].ID = "gpu-" + devices[index].ID
	}
	return &acceleratorv1alpha1.AcceleratorEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: poolName},
			Address: name + ".example",
			Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "cricket", Version: "v1"},
			Devices: devices,
		},
		Status: acceleratorv1alpha1.AcceleratorEndpointStatus{Health: acceleratorv1alpha1.DeviceHealthHealthy, Confidence: "Observed", ActiveSessions: new(int32), ObservedAt: timePointerForTest(), Conditions: []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1,
		}}},
	}
}

func timePointerForTest() *metav1.Time { now := metav1.Now(); return &now }

func TestUnhealthyEndpointRetainsAdvertisedAndReservedCapacity(t *testing.T) {
	pool := &acceleratorv1alpha1.AcceleratorPool{ObjectMeta: metav1.ObjectMeta{Name: "remote"}, Spec: acceleratorv1alpha1.AcceleratorPoolSpec{Location: acceleratorv1alpha1.ExecutionModeRemote, Remote: &acceleratorv1alpha1.RemotePoolConfig{}}}
	endpoint := capacityEndpoint("held", pool.Name, 2)
	endpoint.Spec.Devices[0].Health = acceleratorv1alpha1.DeviceHealthUnhealthy
	lease := &acceleratorv1alpha1.AcceleratorLease{ObjectMeta: metav1.ObjectMeta{Name: "lease", Namespace: "default"}, Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{Mode: acceleratorv1alpha1.ExecutionModeRemote, Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: endpoint.Name, Exclusive: true}}}, Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseReleasing}}
	kube := fake.NewClientBuilder().WithScheme(capacityTestScheme(t)).WithObjects(pool, endpoint, lease).Build()
	capacity, ready, err := SummarizePoolCapacity(context.Background(), kube, pool, "cricket", nil)
	if err != nil {
		t.Fatal(err)
	}
	if capacity.Total != 2 || capacity.Reserved != 2 || capacity.Available != 0 || ready != 0 {
		t.Fatalf("unhealthy reservation disappeared: %+v ready=%d", capacity, ready)
	}
}
