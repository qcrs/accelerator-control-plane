package inventory

import (
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestLocalAccountingAvoidsBoundLeaseDoubleCount(t *testing.T) {
	resourceName := corev1.ResourceName("nvidia.com/gpu")
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-uid")},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{resourceName: resource.MustParse("1")},
				},
			}},
		},
	}
	leases := []acceleratorv1alpha1.AcceleratorLease{
		{
			Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
				PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "local"},
				Mode:    acceleratorv1alpha1.ExecutionModeLocal,
				Targets: []acceleratorv1alpha1.AllocationTarget{{NodeName: "node-a", Count: 1}},
			},
			Status: acceleratorv1alpha1.AcceleratorLeaseStatus{
				Phase:  acceleratorv1alpha1.AllocationPhaseBound,
				PodRef: &corev1.ObjectReference{UID: types.UID("pod-uid")},
			},
		},
		{
			Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
				PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "local"},
				Mode:    acceleratorv1alpha1.ExecutionModeLocal,
				Targets: []acceleratorv1alpha1.AllocationTarget{{NodeName: "node-a", Count: 2}},
			},
			Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseReserved},
		},
	}

	if got := RequestedOnNode([]corev1.Pod{pod}, "node-a", resourceName); got != 1 {
		t.Fatalf("expected one requested GPU, got %d", got)
	}
	poolResources := map[string]corev1.ResourceName{"local": resourceName}
	if got := ReservedOnNode(leases, []corev1.Pod{pod}, poolResources, resourceName, "node-a"); got != 2 {
		t.Fatalf("expected only unbound reservation to count, got %d", got)
	}
}

func TestLocalAccountingCountsReservationsAcrossPoolsSharingAResource(t *testing.T) {
	resourceName := corev1.ResourceName("nvidia.com/gpu")
	leases := []acceleratorv1alpha1.AcceleratorLease{
		{
			Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
				PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "pool-a"},
				Mode:    acceleratorv1alpha1.ExecutionModeLocal,
				Targets: []acceleratorv1alpha1.AllocationTarget{{NodeName: "node-a", Count: 1}},
			},
			Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseReserved},
		},
		{
			Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
				PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "different-resource"},
				Mode:    acceleratorv1alpha1.ExecutionModeLocal,
				Targets: []acceleratorv1alpha1.AllocationTarget{{NodeName: "node-a", Count: 3}},
			},
			Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseReserved},
		},
	}
	poolResources := map[string]corev1.ResourceName{
		"pool-a":             resourceName,
		"pool-b":             resourceName,
		"different-resource": "example.com/fpga",
	}
	if got := ReservedOnNode(leases, nil, poolResources, resourceName, "node-a"); got != 1 {
		t.Fatalf("expected reservation from another pool sharing the resource to count, got %d", got)
	}
}

func TestOccupiedRemoteCapacityTracksExclusiveAndDeviceLeases(t *testing.T) {
	leases := []acceleratorv1alpha1.AcceleratorLease{
		{
			Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
				Mode:    acceleratorv1alpha1.ExecutionModeRemote,
				Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "a", DeviceIDs: []string{"gpu-0"}}},
			},
			Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseBound},
		},
		{
			Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
				Mode:    acceleratorv1alpha1.ExecutionModeRemote,
				Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "b", DeviceIDs: []string{"gpu-1"}, Exclusive: true}},
			},
			Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhasePending},
		},
		{
			Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
				Mode:    acceleratorv1alpha1.ExecutionModeRemote,
				Targets: []acceleratorv1alpha1.AllocationTarget{{EndpointName: "c", DeviceIDs: []string{"gpu-2"}}},
			},
			Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseReleased},
		},
	}
	occupied := OccupiedRemoteCapacity(leases)
	if _, exists := occupied.Devices["a"]["gpu-0"]; !exists {
		t.Fatal("bound device was not marked occupied")
	}
	if _, exists := occupied.Endpoints["b"]; !exists {
		t.Fatal("exclusive endpoint was not marked occupied")
	}
	if _, exists := occupied.Devices["c"]["gpu-2"]; exists {
		t.Fatal("released device remained occupied")
	}
}

func TestNewLeaseConsumesCapacityBeforeStatusInitialization(t *testing.T) {
	lease := acceleratorv1alpha1.AcceleratorLease{
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			Mode: acceleratorv1alpha1.ExecutionModeRemote,
			Targets: []acceleratorv1alpha1.AllocationTarget{{
				EndpointName: "a", DeviceIDs: []string{"gpu-0"}, Exclusive: true,
			}},
		},
	}
	if _, exists := OccupiedRemoteCapacity([]acceleratorv1alpha1.AcceleratorLease{lease}).Endpoints["a"]; !exists {
		t.Fatal("new lease must reserve capacity before its status is initialized")
	}
}

func TestNodeMatchesPodSchedulingConstraints(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{"zone": "east", "generation": "4"},
		},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: "accelerator", Value: "gpu", Effect: corev1.TaintEffectNoSchedule,
		}}},
	}
	podSpec := &corev1.PodSpec{
		NodeSelector: map[string]string{"zone": "east"},
		Tolerations: []corev1.Toleration{{
			Key: "accelerator", Operator: corev1.TolerationOpEqual, Value: "gpu", Effect: corev1.TaintEffectNoSchedule,
		}},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "generation", Operator: corev1.NodeSelectorOpGt, Values: []string{"3"},
					}},
				}},
			},
		}},
	}
	if !nodeMatchesPod(node, podSpec) {
		t.Fatal("expected node to satisfy Pod scheduling constraints")
	}
	podSpec.Tolerations = nil
	if nodeMatchesPod(node, podSpec) {
		t.Fatal("expected hard taint to reject Pod without toleration")
	}
}

func TestQuantityAccountingPreservesScaleAndPodSchedulingSemantics(t *testing.T) {
	resourceName := corev1.ResourceName("example.com/gpu-share")
	pod := corev1.Pod{Spec: corev1.PodSpec{
		NodeName: "node-a",
		Containers: []corev1.Container{
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{resourceName: resource.MustParse("500m")}}},
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{resourceName: resource.MustParse("250m")}}},
		},
		InitContainers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{resourceName: resource.MustParse("1")}},
		}},
		Overhead: corev1.ResourceList{resourceName: resource.MustParse("250m")},
	}}
	wanted := resource.MustParse("1250m")
	got := RequestedQuantityOnNode([]corev1.Pod{pod}, "node-a", resourceName)
	if got.Cmp(wanted) != 0 {
		t.Fatalf("quantity accounting returned %s, want %s", got.String(), wanted.String())
	}
}

func TestLocalLeaseResourceSnapshotSurvivesPoolDeletionAndRelease(t *testing.T) {
	resourceName := corev1.ResourceName("example.com/gpu-share")
	lease := acceleratorv1alpha1.AcceleratorLease{
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "deleted-pool"},
			Mode:    acceleratorv1alpha1.ExecutionModeLocal,
			Targets: []acceleratorv1alpha1.AllocationTarget{{
				NodeName: "node-a", Count: 2, ResourceName: resourceName,
				ResourceForm: acceleratorv1alpha1.ResourceFormShared,
			}},
		},
		Status: acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: acceleratorv1alpha1.AllocationPhaseReserved},
	}
	if got := ReservedOnNode([]acceleratorv1alpha1.AcceleratorLease{lease}, nil, nil, resourceName, "node-a"); got != 2 {
		t.Fatalf("snapshotted resource reservation disappeared with its Pool, got %d", got)
	}
	lease.Status.Phase = acceleratorv1alpha1.AllocationPhaseReleased
	if got := ReservedOnNode([]acceleratorv1alpha1.AcceleratorLease{lease}, nil, nil, resourceName, "node-a"); got != 0 {
		t.Fatalf("released reservation still consumes %d units", got)
	}
}

// Restartable init containers overlap subsequent init stages and the application.
func TestRestartableInitResourceAccounting(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	container := func(q string, sidecar bool) corev1.Container {
		c := corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(q)}}}
		if sidecar {
			c.RestartPolicy = &always
		}
		return c
	}
	for _, tc := range []struct {
		name string
		init []corev1.Container
		want string
	}{
		{"application overlaps sidecars", []corev1.Container{container("200m", true), container("300m", true)}, "1600m"},
		{"init overlaps earlier sidecar", []corev1.Container{container("500m", true), container("2", false)}, "2600m"},
		{"later sidecar does not overlap earlier init", []corev1.Container{container("2", false), container("500m", true)}, "2100m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{container("1", false)}, InitContainers: tc.init, Overhead: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}}
			got := podRequestQuantity(pod, corev1.ResourceCPU)
			want := resource.MustParse(tc.want)
			if got.Cmp(want) != 0 {
				t.Fatalf("got %s want %s", got.String(), want.String())
			}
		})
	}
}
