package inventory

import (
	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func RequestedOnNode(pods []corev1.Pod, nodeName string, resourceName corev1.ResourceName) int64 {
	var requested int64
	for index := range pods {
		pod := &pods[index]
		if pod.Spec.NodeName != nodeName || podIsTerminal(pod) {
			continue
		}
		requested += podRequest(pod, resourceName)
	}
	return requested
}

func ReservedOnNode(
	leases []acceleratorv1alpha1.AcceleratorLease,
	pods []corev1.Pod,
	poolName,
	nodeName string,
) int64 {
	podsByUID := make(map[string]*corev1.Pod, len(pods))
	for index := range pods {
		pod := &pods[index]
		podsByUID[string(pod.UID)] = pod
	}

	var reserved int64
	for index := range leases {
		lease := &leases[index]
		if !leaseConsumesCapacity(lease) ||
			lease.Spec.Mode != acceleratorv1alpha1.ExecutionModeLocal ||
			lease.Spec.PoolRef.Name != poolName {
			continue
		}
		if lease.Status.PodRef != nil {
			if pod := podsByUID[string(lease.Status.PodRef.UID)]; pod != nil &&
				pod.Spec.NodeName == nodeName && !podIsTerminal(pod) {
				continue
			}
		}
		for _, target := range lease.Spec.Targets {
			if target.NodeName == nodeName {
				reserved += int64(target.Count)
			}
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
	var applicationSum int64
	for index := range pod.Spec.Containers {
		applicationSum += containerRequest(pod.Spec.Containers[index], resourceName)
	}

	var largestInit int64
	for index := range pod.Spec.InitContainers {
		requested := containerRequest(pod.Spec.InitContainers[index], resourceName)
		if requested > largestInit {
			largestInit = requested
		}
	}
	if largestInit > applicationSum {
		applicationSum = largestInit
	}
	if quantity, exists := pod.Spec.Overhead[resourceName]; exists {
		applicationSum += quantity.Value()
	}
	return applicationSum
}

func containerRequest(container corev1.Container, resourceName corev1.ResourceName) int64 {
	if quantity, exists := container.Resources.Requests[resourceName]; exists {
		return quantity.Value()
	}
	if quantity, exists := container.Resources.Limits[resourceName]; exists {
		return quantity.Value()
	}
	return 0
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
