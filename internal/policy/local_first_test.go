package policy

import (
	"context"
	"testing"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
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
			name:    "remote endpoints cannot aggregate",
			request: gpuRequest(2),
			candidates: []contracts.Candidate{
				remoteCandidate("remote", "endpoint-b", 10, device("gpu-b", "24Gi")),
				remoteCandidate("remote", "endpoint-a", 10, device("gpu-a", "24Gi")),
			},
			wantErrorKind: contracts.ErrorKindUnschedulable,
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
			name:    "exclusive remote endpoints are never aggregated",
			request: gpuRequest(2),
			candidates: []contracts.Candidate{
				exclusiveRemoteCandidate("remote", "endpoint-a", 10, device("gpu-a", "24Gi")),
				exclusiveRemoteCandidate("remote", "endpoint-b", 10, device("gpu-b", "24Gi")),
			},
			wantErrorKind: contracts.ErrorKindUnschedulable,
		},
		{
			name:    "exclusive endpoint cannot expose unrequested devices",
			request: gpuRequest(1),
			candidates: []contracts.Candidate{
				exclusiveRemoteCandidate("remote", "endpoint-a", 10,
					device("gpu-a", "24Gi"), device("gpu-b", "24Gi")),
			},
			wantErrorKind: contracts.ErrorKindUnschedulable,
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
		{
			name:    "Auto falls back to remote when local capacity is short",
			request: gpuRequest(2),
			candidates: []contracts.Candidate{
				localCandidate("local", "node-a", 100, 1, "24Gi"),
				remoteCandidate("remote", "endpoint-a", 1, device("gpu-a", "24Gi"), device("gpu-b", "24Gi")),
			},
			wantMode: acceleratorv1alpha1.ExecutionModeRemote, wantPool: "remote", wantTargets: 1,
		},
		{
			name: "LocalOnly never selects remote when local capacity is short",
			request: func() contracts.SelectionRequest {
				request := gpuRequest(2)
				request.Placement.Mode = acceleratorv1alpha1.PlacementModeLocalOnly
				return request
			}(),
			candidates: []contracts.Candidate{
				localCandidate("local", "node-a", 100, 1, "24Gi"),
				remoteCandidate("remote", "endpoint-a", 1, device("gpu-a", "24Gi"), device("gpu-b", "24Gi")),
			},
			wantErrorKind: contracts.ErrorKindUnschedulable,
		},
		{
			name: "RemoteOnly ignores an eligible local candidate",
			request: func() contracts.SelectionRequest {
				request := gpuRequest(1)
				request.Placement.Mode = acceleratorv1alpha1.PlacementModeRemoteOnly
				return request
			}(),
			candidates: []contracts.Candidate{
				localCandidate("local", "node-a", 100, 2, "24Gi"),
				remoteCandidate("remote", "endpoint-a", 1, device("gpu-a", "24Gi")),
			},
			wantMode: acceleratorv1alpha1.ExecutionModeRemote, wantPool: "remote", wantTargets: 1,
		},
		{
			name: "resource form and profile prevent whole device and MIG units from mixing",
			request: contracts.SelectionRequest{Accelerator: acceleratorv1alpha1.AcceleratorRequest{
				DeviceType: acceleratorv1alpha1.DeviceKindGPU, Vendor: "nvidia", Count: 1,
				ResourceForm: acceleratorv1alpha1.ResourceFormMIG, Profile: "1g.10gb",
			}},
			candidates: []contracts.Candidate{
				localCandidate("whole", "node-a", 100, 1, "40Gi"),
				localResourceCandidate("mig", "node-a", 10, 2, "10Gi", "nvidia.com/mig-1g.10gb", acceleratorv1alpha1.ResourceFormMIG, "1g.10gb"),
			},
			wantMode: acceleratorv1alpha1.ExecutionModeLocal, wantPool: "mig", wantTargets: 1,
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

func TestFailedTargetExclusionIsScopedToPool(t *testing.T) {
	request := gpuRequest(1)
	request.Exclusions = []contracts.CandidateExclusion{{
		Mode:       acceleratorv1alpha1.ExecutionModeLocal,
		PoolName:   "failed-pool",
		TargetName: "node-a",
	}}
	decision, err := NewLocalFirst().Select(context.Background(), request, []contracts.Candidate{
		localCandidate("failed-pool", "node-a", 100, 1, "24Gi"),
		localCandidate("healthy-pool", "node-a", 10, 1, "24Gi"),
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if decision.PoolName != "healthy-pool" || len(decision.Targets) != 1 || decision.Targets[0].NodeName != "node-a" {
		t.Fatalf("failure in one Pool excluded the same Node from another Pool: %#v", decision)
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
		ResourceName: "nvidia.com/gpu",
		ResourceForm: acceleratorv1alpha1.ResourceFormWholeDevice,
		Vendor:       "nvidia",
		Available:    available,
		Ready:        true,
	}
	if memory != "" {
		candidate.DeviceMemory = resource.MustParse(memory)
	}
	return candidate
}

func localResourceCandidate(
	pool, node string,
	priority, available int32,
	memory string,
	resourceName corev1.ResourceName,
	form acceleratorv1alpha1.ResourceForm,
	profile string,
) contracts.Candidate {
	candidate := localCandidate(pool, node, priority, available, memory)
	candidate.ResourceName = resourceName
	candidate.ResourceForm = form
	candidate.Profile = profile
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
		Exclusive:    true,
		Available:    int32(len(devices)),
		Devices:      devices,
		Ready:        true,
	}
}

func exclusiveRemoteCandidate(pool, endpoint string, priority int32, devices ...contracts.Device) contracts.Candidate {
	candidate := remoteCandidate(pool, endpoint, priority, devices...)
	candidate.Exclusive = true
	return candidate
}

func device(id, memory string) contracts.Device {
	return contracts.Device{
		ID:           id,
		Vendor:       "nvidia",
		Model:        "A100",
		Memory:       resource.MustParse(memory),
		ResourceForm: acceleratorv1alpha1.ResourceFormWholeDevice,
	}
}

func TestDeviceCandidatesAreRejected(t *testing.T) {
	candidate := remoteCandidate("remote", "endpoint", 1, device("gpu", "24Gi"))
	candidate.Exclusive = false
	_, err := NewLocalFirst().Select(context.Background(), gpuRequest(1), []contracts.Candidate{candidate})
	if !contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable) {
		t.Fatalf("unsupported device allocation selected: %v", err)
	}
}

func TestAutoSelectsRemoteWhenLocalHasNoGPUOrNPU(t *testing.T) {
	for _, kind := range []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU, acceleratorv1alpha1.DeviceKindNPU} {
		t.Run(string(kind), func(t *testing.T) {
			request := gpuRequest(1)
			request.Accelerator.DeviceType = kind
			local := localCandidate("local", "no-device-node", 1, 0, "24Gi")
			local.DeviceType = kind
			remote := remoteCandidate("remote", "has-device-server", 1, device("0", "24Gi"))
			remote.DeviceType = kind
			decision, err := NewLocalFirst().Select(context.Background(), request, []contracts.Candidate{local, remote})
			if err != nil || decision.Mode != acceleratorv1alpha1.ExecutionModeRemote || len(decision.Targets) != 1 || !decision.Targets[0].Exclusive {
				t.Fatalf("local without %s did not select remote: %+v %v", kind, decision, err)
			}
		})
	}
}

func TestExclusiveEndpointCannotSelectOnlyCompatibleSubset(t *testing.T) {
	candidate := remoteCandidate("remote", "mixed", 1, device("matches", "24Gi"), device("other", "24Gi"))
	candidate.Devices[1].Vendor = "other-vendor"
	_, err := NewLocalFirst().Select(context.Background(), gpuRequest(1), []contracts.Candidate{candidate})
	if !contracts.IsErrorKind(err, contracts.ErrorKindUnschedulable) {
		t.Fatalf("selected a compatible subset of an exclusive endpoint: %v", err)
	}
}
