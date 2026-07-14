package agent

import (
	"context"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

type UnsupportedProbe struct{}

func (UnsupportedProbe) Probe(_ context.Context) ([]acceleratorv1alpha1.DeviceInfo, acceleratorv1alpha1.RuntimeInfo, error) {
	return nil, acceleratorv1alpha1.RuntimeInfo{Driver: "lupine"}, contracts.NewError(
		contracts.ErrorKindUnsupported,
		"probe remote devices",
		"GPU/NPU discovery is not implemented",
	)
}
