package allocation

import (
	"context"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type KubernetesEndpointResolver struct {
	Client client.Reader
}

func (r *KubernetesEndpointResolver) Resolve(ctx context.Context, endpointName string) (contracts.RemoteEndpoint, error) {
	if r.Client == nil {
		return contracts.RemoteEndpoint{}, contracts.NewError(contracts.ErrorKindInvalid, "resolve endpoint", "Kubernetes client is nil")
	}
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: endpointName}, endpoint); err != nil {
		if apierrors.IsNotFound(err) {
			return contracts.RemoteEndpoint{}, contracts.WrapError(contracts.ErrorKindConflict, "resolve endpoint", "selected endpoint no longer exists", err)
		}
		return contracts.RemoteEndpoint{}, err
	}
	if condition := meta.FindStatusCondition(endpoint.Status.Conditions, "Configured"); condition != nil {
		if condition.ObservedGeneration != endpoint.Generation {
			return contracts.RemoteEndpoint{}, contracts.NewError(contracts.ErrorKindConflict, "resolve endpoint", fmt.Sprintf("selected endpoint Configured condition is stale for generation %d", endpoint.Generation))
		}
		if condition.Status != metav1.ConditionTrue {
			return contracts.RemoteEndpoint{}, contracts.NewError(contracts.ErrorKindConflict, "resolve endpoint", fmt.Sprintf("selected endpoint is not configured: %s", condition.Message))
		}
	}
	allEndpoints := &acceleratorv1alpha1.AcceleratorEndpointList{}
	if err := r.Client.List(ctx, allEndpoints); err != nil {
		return contracts.RemoteEndpoint{}, contracts.WrapError(contracts.ErrorKindTransient, "resolve endpoint", "list endpoints for device identity validation", err)
	}
	selectedUUIDs := map[string]struct{}{}
	for _, device := range endpoint.Spec.Devices {
		if device.UUID != "" {
			selectedUUIDs[device.UUID] = struct{}{}
		}
	}
	for index := range allEndpoints.Items {
		other := &allEndpoints.Items[index]
		if other.Name == endpoint.Name {
			continue
		}
		for _, device := range other.Spec.Devices {
			if _, duplicate := selectedUUIDs[device.UUID]; device.UUID != "" && duplicate {
				return contracts.RemoteEndpoint{}, contracts.NewError(contracts.ErrorKindConflict, "resolve endpoint", fmt.Sprintf("device UUID %q is also declared by endpoint %q", device.UUID, other.Name))
			}
		}
	}
	resolved := contracts.RemoteEndpoint{
		Name:          endpoint.Name,
		PoolName:      endpoint.Spec.PoolRef.Name,
		Address:       endpoint.Spec.Address,
		Runtime:       endpoint.Spec.Runtime,
		Devices:       copyDeviceInfo(endpoint.Spec.Devices),
		Parameters:    copyParameters(endpoint.Spec.Parameters),
		Unschedulable: endpoint.Spec.Unschedulable,
	}
	if condition := meta.FindStatusCondition(endpoint.Status.Conditions, "Ready"); condition != nil {
		resolved.Readiness = &contracts.EndpointReadiness{
			Ready:   condition.ObservedGeneration == endpoint.Generation && condition.Status == metav1.ConditionTrue,
			Reason:  condition.Reason,
			Message: condition.Message,
		}
		if condition.ObservedGeneration != endpoint.Generation {
			resolved.Readiness.Reason = "StaleCondition"
			resolved.Readiness.Message = fmt.Sprintf("Ready condition is stale for generation %d", endpoint.Generation)
		}
	}

	if ready, reason := contracts.EndpointObservedReady(endpoint, time.Now()); !ready && (resolved.Readiness == nil || resolved.Readiness.Ready) {
		resolved.Readiness = &contracts.EndpointReadiness{Ready: false, Reason: "ObservationNotReady", Message: reason}
	}
	return resolved, nil
}

func copyDeviceInfo(source []acceleratorv1alpha1.DeviceInfo) []acceleratorv1alpha1.DeviceInfo {
	if source == nil {
		return nil
	}
	result := make([]acceleratorv1alpha1.DeviceInfo, len(source))
	for index := range source {
		result[index] = source[index]
		result[index].Memory = source[index].Memory.DeepCopy()
		result[index].Attributes = copyParameters(source[index].Attributes)
	}
	return result
}
