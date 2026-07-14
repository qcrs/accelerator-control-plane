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
	if got := ReservedOnNode(leases, []corev1.Pod{pod}, "local", "node-a"); got != 2 {
		t.Fatalf("expected only unbound reservation to count, got %d", got)
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
