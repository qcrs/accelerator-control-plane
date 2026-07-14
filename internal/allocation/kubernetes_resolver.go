package allocation

import (
	"context"
	"fmt"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type KubernetesEndpointResolver struct {
	Client client.Reader
}

func (r *KubernetesEndpointResolver) ControlAddress(ctx context.Context, endpointName string) (string, error) {
	if r.Client == nil {
		return "", contracts.NewError(contracts.ErrorKindInvalid, "resolve endpoint", "Kubernetes client is nil")
	}
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: endpointName}, endpoint); err != nil {
		return "", err
	}
	if endpoint.Status.ControlAddress == "" {
		return "", fmt.Errorf("endpoint %q has no control address", endpointName)
	}
	return endpoint.Status.ControlAddress, nil
}
