package controller

import (
	"fmt"
	"strings"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func validateWorkload(workload *acceleratorv1alpha1.AcceleratorWorkload, managedResources map[corev1.ResourceName]struct{}) error {
	if workload == nil {
		return fmt.Errorf("workload is nil")
	}
	request := workload.Spec.Accelerator
	if request.DeviceType != acceleratorv1alpha1.DeviceKindGPU && request.DeviceType != acceleratorv1alpha1.DeviceKindNPU {
		return fmt.Errorf("unsupported deviceType %q", request.DeviceType)
	}
	if request.Count < 1 || request.Count > 128 {
		return fmt.Errorf("accelerator count must be between 1 and 128")
	}
	if request.Memory.Sign() < 0 {
		return fmt.Errorf("accelerator memory cannot be negative")
	}
	if len(workload.Spec.PodTemplate.Spec.Containers) == 0 {
		return fmt.Errorf("podTemplate must contain at least one regular container")
	}
	if request.ContainerName != "" && !containerExists(workload.Spec.PodTemplate.Spec.Containers, request.ContainerName) {
		return fmt.Errorf("accelerator containerName %q does not exist", request.ContainerName)
	}
	if workload.Spec.Placement.ReservationTTL.Duration < 0 {
		return fmt.Errorf("reservationTTL cannot be negative")
	}
	if workload.Spec.Placement.ReservationTTL.Duration > 0 && workload.Spec.Placement.ReservationTTL.Duration < time.Second {
		return fmt.Errorf("reservationTTL must be at least one second")
	}
	if err := validatePlacement(workload.Spec.Placement); err != nil {
		return err
	}
	if err := validateUniqueStrings("models", request.Models); err != nil {
		return err
	}
	containers := make([]corev1.Container, 0, len(workload.Spec.PodTemplate.Spec.Containers)+len(workload.Spec.PodTemplate.Spec.InitContainers))
	containers = append(containers, workload.Spec.PodTemplate.Spec.Containers...)
	containers = append(containers, workload.Spec.PodTemplate.Spec.InitContainers...)
	for _, container := range containers {
		for resourceName := range managedResources {
			if _, exists := container.Resources.Requests[resourceName]; exists {
				return fmt.Errorf("container %q must not request managed resource %q; declare it in spec.accelerator", container.Name, resourceName)
			}
			if _, exists := container.Resources.Limits[resourceName]; exists {
				return fmt.Errorf("container %q must not limit managed resource %q; declare it in spec.accelerator", container.Name, resourceName)
			}
		}
	}
	return nil
}

func validatePlacement(placement acceleratorv1alpha1.PlacementPolicy) error {
	switch placement.Mode {
	case "", acceleratorv1alpha1.PlacementModeAuto,
		acceleratorv1alpha1.PlacementModeLocalOnly,
		acceleratorv1alpha1.PlacementModeRemoteOnly:
	default:
		return fmt.Errorf("unsupported placement mode %q", placement.Mode)
	}
	seen := map[acceleratorv1alpha1.ExecutionMode]struct{}{}
	for _, mode := range placement.Order {
		if mode != acceleratorv1alpha1.ExecutionModeLocal && mode != acceleratorv1alpha1.ExecutionModeRemote {
			return fmt.Errorf("unsupported placement order entry %q", mode)
		}
		if _, exists := seen[mode]; exists {
			return fmt.Errorf("placement order entry %q is duplicated", mode)
		}
		seen[mode] = struct{}{}
	}
	if placement.Mode != "" && placement.Mode != acceleratorv1alpha1.PlacementModeAuto && len(placement.Order) > 0 {
		return fmt.Errorf("placement order is only valid in Auto mode")
	}
	return nil
}

func validateUniqueStrings(field string, values []string) error {
	seen := map[string]struct{}{}
	for _, value := range values {
		normalized := strings.ToLower(value)
		if _, exists := seen[normalized]; exists {
			return fmt.Errorf("%s entry %q is duplicated", field, value)
		}
		seen[normalized] = struct{}{}
	}
	return nil
}

func containerExists(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}
