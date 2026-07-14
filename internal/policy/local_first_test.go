package policy

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestLocalFirstSelect(t *testing.T) {
	tests := []struct {
		name          string
		request       contracts.SelectionRequest
		candidates    []contracts.Candidate
		wantMode      acceleratorv1alpha1.ExecutionMode
		wantPool      string
		wantTargets   int
		wantErrorKind contracts.ErrorKind
	}{
		{
			name:    "local wins over higher priority remote",
			request: gpuRequest(1),
			candidates: []contracts.Candidate{
				remoteCandidate("remote", "endpoint-a", 100, device("gpu-a", "24Gi")),
				localCandidate("local", "node-a", 1, 1, "24Gi"),
			},
			wantMode:    acceleratorv1alpha1.ExecutionModeLocal,
			wantPool:    "local",
			wantTargets: 1,
		},
		{
			name:    "remote pool aggregates ordered endpoints",
			request: gpuRequest(2),
			candidates: []contracts.Candidate{
				remoteCandidate("remote", "endpoint-b", 10, device("gpu-b", "24Gi")),
				remoteCandidate("remote", "endpoint-a", 10, device("gpu-a", "24Gi")),
			},
			wantMode:    acceleratorv1alpha1.ExecutionModeRemote,
			wantPool:    "remote",
			wantTargets: 2,
		},
		{
			name:    "remote pool prefers one endpoint when it fits",
			request: gpuRequest(2),
			candidates: []contracts.Candidate{
				remoteCandidate("remote", "endpoint-small", 10, device("gpu-a", "24Gi")),
				remoteCandidate("remote", "endpoint-fit", 10, device("gpu-b", "24Gi"), device("gpu-c", "24Gi")),
			},
			wantMode:    acceleratorv1alpha1.ExecutionModeRemote,
			wantPool:    "remote",
			wantTargets: 1,
		},
		{
			name: "local memory must be known and sufficient",
			request: contracts.SelectionRequest{
				Accelerator: acceleratorv1alpha1.AcceleratorRequest{
					DeviceType: acceleratorv1alpha1.DeviceKindGPU,
					Vendor:     "nvidia",
					Count:      1,
					Memory:     resource.MustParse("16Gi"),
				},
			},
			candidates: []contracts.Candidate{
				localCandidate("local", "node-a", 100, 1, ""),
				remoteCandidate("remote", "endpoint-a", 1, device("gpu-a", "24Gi")),
			},
			wantMode:    acceleratorv1alpha1.ExecutionModeRemote,
			wantPool:    "remote",
			wantTargets: 1,
		},
		{
			name:    "local vendor must be known when requested",
			request: gpuRequest(1),
			candidates: []contracts.Candidate{
				{
					ID: "local/local/node-a", ProviderName: "local-provider", PoolName: "local",
					Mode: acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
					NodeName: "node-a", Available: 1, Ready: true,
				},
				remoteCandidate("remote", "endpoint-a", 1, device("gpu-a", "24Gi")),
			},
			wantMode:    acceleratorv1alpha1.ExecutionModeRemote,
			wantPool:    "remote",
			wantTargets: 1,
		},
		{
			name:    "unschedulable when capacity is short",
			request: gpuRequest(2),
			candidates: []contracts.Candidate{
				localCandidate("local", "node-a", 1, 1, "24Gi"),
			},
			wantErrorKind: contracts.ErrorKindUnschedulable,
		},
	}

	engine := NewLocalFirst()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := engine.Select(context.Background(), test.request, test.candidates)
			if test.wantErrorKind != "" {
				if !contracts.IsErrorKind(err, test.wantErrorKind) {
					t.Fatalf("expected %s error, got %v", test.wantErrorKind, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select returned error: %v", err)
			}
			if decision.Mode != test.wantMode || decision.PoolName != test.wantPool {
				t.Fatalf("unexpected decision: %#v", decision)
			}
			if len(decision.Targets) != test.wantTargets {
				t.Fatalf("expected %d targets, got %d", test.wantTargets, len(decision.Targets))
			}
		})
	}
}

func TestLocalFirstRejectsDuplicateOrder(t *testing.T) {
	request := gpuRequest(1)
	request.Placement.Order = []acceleratorv1alpha1.ExecutionMode{
		acceleratorv1alpha1.ExecutionModeLocal,
		acceleratorv1alpha1.ExecutionModeLocal,
	}
	_, err := NewLocalFirst().Select(context.Background(), request, nil)
	if !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("expected invalid error, got %v", err)
	}
}

func gpuRequest(count int32) contracts.SelectionRequest {
	return contracts.SelectionRequest{
		Accelerator: acceleratorv1alpha1.AcceleratorRequest{
			DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Vendor:     "nvidia",
			Count:      count,
		},
	}
}

func localCandidate(pool, node string, priority, available int32, memory string) contracts.Candidate {
	candidate := contracts.Candidate{
		ID:           "local/" + pool + "/" + node,
		ProviderName: "local-provider",
		PoolName:     pool,
		PoolPriority: priority,
		Mode:         acceleratorv1alpha1.ExecutionModeLocal,
		DeviceType:   acceleratorv1alpha1.DeviceKindGPU,
		NodeName:     node,
		Vendor:       "nvidia",
		Available:    available,
		Ready:        true,
	}
	if memory != "" {
		candidate.DeviceMemory = resource.MustParse(memory)
	}
	return candidate
}

func remoteCandidate(pool, endpoint string, priority int32, devices ...contracts.Device) contracts.Candidate {
	return contracts.Candidate{
		ID:           "remote/" + pool + "/" + endpoint,
		ProviderName: "remote-provider",
		PoolName:     pool,
		PoolPriority: priority,
		Mode:         acceleratorv1alpha1.ExecutionModeRemote,
		DeviceType:   acceleratorv1alpha1.DeviceKindGPU,
		EndpointName: endpoint,
		Available:    int32(len(devices)),
		Devices:      devices,
		Ready:        true,
	}
}

func device(id, memory string) contracts.Device {
	return contracts.Device{
		ID:     id,
		Vendor: "nvidia",
		Model:  "A100",
		Memory: resource.MustParse(memory),
	}
}
