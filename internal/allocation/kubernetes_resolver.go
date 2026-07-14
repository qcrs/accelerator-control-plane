package allocation

import (
	"context"

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
			Ready:   condition.Status == metav1.ConditionTrue,
			Reason:  condition.Reason,
			Message: condition.Message,
		}
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
