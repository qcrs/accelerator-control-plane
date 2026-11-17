package controller

import (
	"fmt"
	"strings"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	if placement.Fallback == nil {
		return nil
	}
	fallback := placement.Fallback
	if fallback.MaxAttempts < 0 || fallback.MaxAttempts == 1 || fallback.MaxAttempts > 8 {
		return fmt.Errorf("fallback maxAttempts must be between 2 and 8")
	}
	if fallback.StartupTimeout.Duration < 0 {
		return fmt.Errorf("fallback startupTimeout cannot be negative")
	}
	if fallback.StartupTimeout.Duration > 0 && fallback.StartupTimeout.Duration < time.Second {
		return fmt.Errorf("fallback startupTimeout must be at least one second")
	}
	if fallback.Backoff.Duration < 0 {
		return fmt.Errorf("fallback backoff cannot be negative")
	}
	if !fallback.Enabled {
		return nil
	}
	if placement.Mode != "" && placement.Mode != acceleratorv1alpha1.PlacementModeAuto {
		return fmt.Errorf("startup fallback requires placement mode Auto")
	}
	if len(placement.Order) == 1 {
		return fmt.Errorf("startup fallback requires both Local and Remote in placement order")
	}
	return nil
}

func validateRuntimePlan(
	plan contracts.RuntimePlan,
	lease *acceleratorv1alpha1.AcceleratorLease,
	managedResources map[corev1.ResourceName]struct{},
) error {
	if lease == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", "allocation Lease is required")
	}
	if plan.Mode == "" || plan.Mode != lease.Spec.Mode {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", fmt.Sprintf("plan mode %q does not match Lease mode %q", plan.Mode, lease.Spec.Mode))
	}
	if lease.Spec.Request.ContainerName != "" && plan.ContainerName != lease.Spec.Request.ContainerName {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", fmt.Sprintf("plan targets container %q instead of %q", plan.ContainerName, lease.Spec.Request.ContainerName))
	}

	switch lease.Spec.Mode {
	case acceleratorv1alpha1.ExecutionModeLocal:
		if len(lease.Spec.Targets) != 1 || lease.Spec.Targets[0].NodeName == "" || lease.Spec.Targets[0].EndpointName != "" {
			return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", "Local mode requires exactly one Node target and no Endpoint target")
		}
		target := lease.Spec.Targets[0]
		if plan.RequiredNodeName != target.NodeName || plan.Resources == nil {
			return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", "Local plan must request resources on the selected Node")
		}
		resourceName := target.ResourceName
		if resourceName == "" {
			resourceName = soleManagedResource(*plan.Resources, managedResources)
		}
		if resourceName == "" || target.Count <= 0 {
			return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", "Local target requires a resource name and positive unit count")
		}
		expected := *resource.NewQuantity(int64(target.Count), resource.DecimalSI)
		if !resourceListContains(plan.Resources.Requests, resourceName, expected) ||
			!resourceListContains(plan.Resources.Limits, resourceName, expected) {
			return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", fmt.Sprintf("Local plan must request and limit %s=%s", resourceName, expected.String()))
		}
	case acceleratorv1alpha1.ExecutionModeRemote:
		for _, target := range lease.Spec.Targets {
			if target.EndpointName == "" || target.NodeName != "" || target.ResourceName != "" || target.Count != 0 {
				return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", "Remote mode requires Endpoint targets without local resource requests")
			}
		}
		if plan.Resources != nil {
			for resourceName := range managedResources {
				if _, exists := plan.Resources.Requests[resourceName]; exists {
					return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", fmt.Sprintf("Remote plan must not request local resource %q", resourceName))
				}
				if _, exists := plan.Resources.Limits[resourceName]; exists {
					return contracts.NewError(contracts.ErrorKindInvalid, "validate runtime plan", fmt.Sprintf("Remote plan must not limit local resource %q", resourceName))
				}
			}
		}
	default:
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate runtime plan", fmt.Sprintf("execution mode %q is unsupported", lease.Spec.Mode))
	}
	return nil
}

func soleManagedResource(resources corev1.ResourceRequirements, managed map[corev1.ResourceName]struct{}) corev1.ResourceName {
	var found corev1.ResourceName
	for name := range resources.Requests {
		if _, isManaged := managed[name]; !isManaged {
			continue
		}
		if found != "" && found != name {
			return ""
		}
		found = name
	}
	return found
}

func resourceListContains(resources corev1.ResourceList, name corev1.ResourceName, expected resource.Quantity) bool {
	quantity, exists := resources[name]
	return exists && quantity.Cmp(expected) == 0
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
