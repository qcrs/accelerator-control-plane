package inventory

import (
	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func RequestedOnNode(pods []corev1.Pod, nodeName string, resourceName corev1.ResourceName) int64 {
	quantity := RequestedQuantityOnNode(pods, nodeName, resourceName)
	return quantity.Value()
}

func RequestedQuantityOnNode(pods []corev1.Pod, nodeName string, resourceName corev1.ResourceName) resource.Quantity {
	requested := *resource.NewQuantity(0, resource.DecimalSI)
	for index := range pods {
		pod := &pods[index]
		if pod.Spec.NodeName != nodeName || podIsTerminal(pod) {
			continue
		}
		requested.Add(podRequestQuantity(pod, resourceName))
	}
	return requested
}

func ReservedOnNode(
	leases []acceleratorv1alpha1.AcceleratorLease,
	pods []corev1.Pod,
	poolResources map[string]corev1.ResourceName,
	resourceName corev1.ResourceName,
	nodeName string,
) int64 {
	quantity := ReservedQuantityOnNode(leases, pods, poolResources, resourceName, nodeName)
	return quantity.Value()
}

func ReservedQuantityOnNode(
	leases []acceleratorv1alpha1.AcceleratorLease,
	pods []corev1.Pod,
	poolResources map[string]corev1.ResourceName,
	resourceName corev1.ResourceName,
	nodeName string,
) resource.Quantity {
	podsByUID := make(map[string]*corev1.Pod, len(pods))
	for index := range pods {
		pod := &pods[index]
		podsByUID[string(pod.UID)] = pod
	}

	reserved := *resource.NewQuantity(0, resource.DecimalSI)
	for index := range leases {
		lease := &leases[index]
		if !leaseConsumesCapacity(lease) ||
			lease.Spec.Mode != acceleratorv1alpha1.ExecutionModeLocal {
			continue
		}
		for _, target := range lease.Spec.Targets {
			targetResource := target.ResourceName
			if targetResource == "" {
				targetResource = poolResources[lease.Spec.PoolRef.Name]
			}
			if target.NodeName != nodeName || targetResource != resourceName || target.Count <= 0 {
				continue
			}
			targetQuantity := *resource.NewQuantity(int64(target.Count), resource.DecimalSI)
			if lease.Status.PodRef != nil {
				if pod := podsByUID[string(lease.Status.PodRef.UID)]; pod != nil &&
					pod.Spec.NodeName == nodeName && !podIsTerminal(pod) {
					podQuantity := podRequestQuantity(pod, resourceName)
					if podQuantity.Cmp(targetQuantity) >= 0 {
						continue
					}
				}
			}
			reserved.Add(targetQuantity)
		}
	}
	return reserved
}

type RemoteOccupancy struct {
	Endpoints map[string]struct{}
	Devices   map[string]map[string]struct{}
}

func OccupiedRemoteCapacity(leases []acceleratorv1alpha1.AcceleratorLease) RemoteOccupancy {
	occupied := RemoteOccupancy{
		Endpoints: map[string]struct{}{},
		Devices:   map[string]map[string]struct{}{},
	}
	for index := range leases {
		lease := &leases[index]
		if !leaseConsumesCapacity(lease) || lease.Spec.Mode != acceleratorv1alpha1.ExecutionModeRemote {
			continue
		}
		for _, target := range lease.Spec.Targets {
			if target.EndpointName == "" {
				continue
			}
			if target.Exclusive {
				occupied.Endpoints[target.EndpointName] = struct{}{}
				continue
			}
			devices := occupied.Devices[target.EndpointName]
			if devices == nil {
				devices = map[string]struct{}{}
				occupied.Devices[target.EndpointName] = devices
			}
			for _, deviceID := range target.DeviceIDs {
				devices[deviceID] = struct{}{}
			}
		}
	}
	return occupied
}

func podRequest(pod *corev1.Pod, resourceName corev1.ResourceName) int64 {
	quantity := podRequestQuantity(pod, resourceName)
	return quantity.Value()
}

func podRequestQuantity(pod *corev1.Pod, resourceName corev1.ResourceName) resource.Quantity {
	applicationSum := *resource.NewQuantity(0, resource.DecimalSI)
	for index := range pod.Spec.Containers {
		applicationSum.Add(containerRequestQuantity(pod.Spec.Containers[index], resourceName))
	}

	largestInit := *resource.NewQuantity(0, resource.DecimalSI)
	for index := range pod.Spec.InitContainers {
		requested := containerRequestQuantity(pod.Spec.InitContainers[index], resourceName)
		if requested.Cmp(largestInit) > 0 {
			largestInit = requested
		}
	}
	if largestInit.Cmp(applicationSum) > 0 {
		applicationSum = largestInit
	}
	if quantity, exists := pod.Spec.Overhead[resourceName]; exists {
		applicationSum.Add(quantity)
	}
	return applicationSum
}

func containerRequest(container corev1.Container, resourceName corev1.ResourceName) int64 {
	quantity := containerRequestQuantity(container, resourceName)
	return quantity.Value()
}

func containerRequestQuantity(container corev1.Container, resourceName corev1.ResourceName) resource.Quantity {
	if quantity, exists := container.Resources.Requests[resourceName]; exists {
		return quantity.DeepCopy()
	}
	if quantity, exists := container.Resources.Limits[resourceName]; exists {
		return quantity.DeepCopy()
	}
	return *resource.NewQuantity(0, resource.DecimalSI)
}

func leaseConsumesCapacity(lease *acceleratorv1alpha1.AcceleratorLease) bool {
	switch lease.Status.Phase {
	case "",
		acceleratorv1alpha1.AllocationPhasePending,
		acceleratorv1alpha1.AllocationPhaseReserved,
		acceleratorv1alpha1.AllocationPhaseBound,
		acceleratorv1alpha1.AllocationPhaseReleasing:
		return true
	default:
		return false
	}
}

func podIsTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}
