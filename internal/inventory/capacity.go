package inventory

import (
	"context"
	"fmt"
	"math"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SummarizePoolCapacity computes an operational snapshot from the same source
// objects used by placement. It is informational: the final remote ownership
// decision is still the atomic coordination Lease claim.
func SummarizePoolCapacity(
	ctx context.Context,
	reader client.Reader,
	pool *acceleratorv1alpha1.AcceleratorPool,
	providerDriver string,
	validator contracts.RemoteEndpointValidator,
) (acceleratorv1alpha1.CapacitySummary, int32, error) {
	if reader == nil || pool == nil {
		return acceleratorv1alpha1.CapacitySummary{}, 0, fmt.Errorf("capacity reader and pool are required")
	}
	switch pool.Spec.Location {
	case acceleratorv1alpha1.ExecutionModeLocal:
		return summarizeLocalPool(ctx, reader, pool)
	case acceleratorv1alpha1.ExecutionModeRemote:
		return summarizeRemotePool(ctx, reader, pool, providerDriver, validator)
	default:
		return acceleratorv1alpha1.CapacitySummary{}, 0, fmt.Errorf("unsupported pool location %q", pool.Spec.Location)
	}
}

func summarizeLocalPool(ctx context.Context, reader client.Reader, pool *acceleratorv1alpha1.AcceleratorPool) (acceleratorv1alpha1.CapacitySummary, int32, error) {
	if pool.Spec.Local == nil || pool.Spec.Local.ResourceName == "" {
		return acceleratorv1alpha1.CapacitySummary{}, 0, fmt.Errorf("local resource name is required")
	}
	var nodes corev1.NodeList
	var pods corev1.PodList
	var leases acceleratorv1alpha1.AcceleratorLeaseList
	var pools acceleratorv1alpha1.AcceleratorPoolList
	for _, objects := range []client.ObjectList{&nodes, &pods, &leases, &pools} {
		if err := reader.List(ctx, objects); err != nil {
			return acceleratorv1alpha1.CapacitySummary{}, 0, fmt.Errorf("list %T for pool capacity: %w", objects, err)
		}
	}
	poolResources := make(map[string]corev1.ResourceName, len(pools.Items))
	for index := range pools.Items {
		candidate := &pools.Items[index]
		if candidate.Spec.Location == acceleratorv1alpha1.ExecutionModeLocal && candidate.Spec.Local != nil {
			poolResources[candidate.Name] = candidate.Spec.Local.ResourceName
		}
	}
	var total, available, consumed int64
	for index := range nodes.Items {
		node := &nodes.Items[index]
		if node.Spec.Unschedulable || !nodeReady(node) || !containsLabels(node.Labels, pool.Spec.Local.NodeSelector) {
			continue
		}
		allocatable := node.Status.Allocatable[pool.Spec.Local.ResourceName]
		capacity := allocatable.DeepCopy()
		if capacity.Sign() <= 0 {
			continue
		}
		used := RequestedQuantityOnNode(pods.Items, node.Name, pool.Spec.Local.ResourceName)
		reserved := ReservedQuantityOnNode(leases.Items, pods.Items, poolResources, pool.Spec.Local.ResourceName, node.Name)
		occupied := used.DeepCopy()
		occupied.Add(reserved)
		if occupied.Cmp(capacity) > 0 {
			occupied = capacity
		}
		free := capacity.DeepCopy()
		free.Sub(used)
		free.Sub(reserved)
		if free.Sign() < 0 {
			free.Set(0)
		}
		total += capacity.Value()
		available += free.Value()
		consumed += occupied.Value()
	}
	return acceleratorv1alpha1.CapacitySummary{
		Total: clampCapacity(total), Available: clampCapacity(available), Reserved: clampCapacity(consumed),
	}, 0, nil
}

func summarizeRemotePool(
	ctx context.Context,
	reader client.Reader,
	pool *acceleratorv1alpha1.AcceleratorPool,
	providerDriver string,
	validator contracts.RemoteEndpointValidator,
) (acceleratorv1alpha1.CapacitySummary, int32, error) {
	if pool.Spec.Remote == nil {
		return acceleratorv1alpha1.CapacitySummary{}, 0, fmt.Errorf("remote pool configuration is required")
	}
	selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.Remote.EndpointSelector)
	if err != nil {
		return acceleratorv1alpha1.CapacitySummary{}, 0, fmt.Errorf("parse endpoint selector: %w", err)
	}
	var endpoints acceleratorv1alpha1.AcceleratorEndpointList
	var leases acceleratorv1alpha1.AcceleratorLeaseList
	if err := reader.List(ctx, &endpoints); err != nil {
		return acceleratorv1alpha1.CapacitySummary{}, 0, fmt.Errorf("list endpoints for pool capacity: %w", err)
	}
	if err := reader.List(ctx, &leases); err != nil {
		return acceleratorv1alpha1.CapacitySummary{}, 0, fmt.Errorf("list leases for pool capacity: %w", err)
	}
	occupied := OccupiedRemoteCapacity(leases.Items)
	duplicates := duplicateDeviceUUIDs(endpoints.Items)
	var total, available, reserved int64
	var readyEndpoints int64
	for index := range endpoints.Items {
		endpoint := &endpoints.Items[index]
		if endpoint.Spec.PoolRef.Name != pool.Name || !selector.Matches(labels.Set(endpoint.Labels)) {
			continue
		}
		healthy := make(map[string]struct{}, len(endpoint.Spec.Devices))
		operational, _ := endpointReady(endpoint, providerDriver)
		if endpoint.Spec.Unschedulable {
			operational = false
		}
		for _, device := range endpoint.Spec.Devices {
			total++ // advertised capacity is not erased by a health failure
			if device.Health == acceleratorv1alpha1.DeviceHealthHealthy {
				healthy[device.ID] = struct{}{}
			}
			if device.UUID != "" {
				if _, duplicate := duplicates[device.UUID]; duplicate {
					operational = false
				}
			}
		}
		if operational && validator != nil {
			if err := validator.ValidateEndpoint(remoteEndpoint(endpoint)); err != nil {
				operational = false
			}
		}
		endpointReserved := int64(0)
		if _, exclusive := occupied.Endpoints[endpoint.Name]; exclusive {
			endpointReserved = int64(len(endpoint.Spec.Devices))
		} else {
			for deviceID := range occupied.Devices[endpoint.Name] {
				if _, exists := healthy[deviceID]; exists {
					endpointReserved++
				}
			}
		}
		reserved += endpointReserved
		if operational {
			readyEndpoints++
			free := int64(len(healthy)) - endpointReserved
			if free > 0 {
				available += free
			}
		}
	}
	return acceleratorv1alpha1.CapacitySummary{
		Total: clampCapacity(total), Available: clampCapacity(available), Reserved: clampCapacity(reserved),
	}, clampCapacity(readyEndpoints), nil
}

func clampCapacity(value int64) int32 {
	if value <= 0 {
		return 0
	}
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(value)
}
