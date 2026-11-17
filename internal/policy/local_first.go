package policy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"k8s.io/apimachinery/pkg/api/resource"
)

const LocalFirstPolicyName = "local-first-fit/v1"

type LocalFirst struct{}

func NewLocalFirst() *LocalFirst { return &LocalFirst{} }

type eligibleCandidate struct {
	candidate contracts.Candidate
	devices   []contracts.Device
}

func (p *LocalFirst) Select(_ context.Context, request contracts.SelectionRequest, candidates []contracts.Candidate) (contracts.Decision, error) {
	order, err := placementOrder(request.Placement)
	if err != nil {
		return contracts.Decision{}, err
	}
	order = preferMode(order, request.PreferredMode)
	rank := make(map[acceleratorv1alpha1.ExecutionMode]int, len(order))
	for index, mode := range order {
		rank[mode] = index
	}

	eligible := make([]eligibleCandidate, 0, len(candidates))
	rejected := 0
	for _, candidate := range candidates {
		if _, enabled := rank[candidate.Mode]; !enabled {
			continue
		}
		if candidateExcluded(candidate, request.Exclusions) {
			rejected++
			continue
		}
		devices, ok := compatibleDevices(request.Accelerator, candidate)
		if !ok {
			rejected++
			continue
		}
		eligible = append(eligible, eligibleCandidate{candidate: candidate, devices: devices})
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		left, right := eligible[i].candidate, eligible[j].candidate
		if rank[left.Mode] != rank[right.Mode] {
			return rank[left.Mode] < rank[right.Mode]
		}
		if left.PoolPriority != right.PoolPriority {
			return left.PoolPriority > right.PoolPriority
		}
		if left.PoolName != right.PoolName {
			return left.PoolName < right.PoolName
		}
		leftFree := freeCount(eligible[i])
		rightFree := freeCount(eligible[j])
		if leftFree != rightFree {
			return leftFree < rightFree
		}
		return left.ID < right.ID
	})

	for _, mode := range order {
		if mode == acceleratorv1alpha1.ExecutionModeLocal {
			if decision, found := selectLocal(request.Accelerator.Count, eligible); found {
				return decision, nil
			}
			continue
		}
		if decision, found := selectRemote(request.Accelerator.Count, eligible); found {
			return decision, nil
		}
	}

	message := fmt.Sprintf("no candidate can satisfy %d %s device(s); %d candidate(s) failed compatibility or health checks", request.Accelerator.Count, request.Accelerator.DeviceType, rejected)
	return contracts.Decision{}, contracts.NewError(contracts.ErrorKindUnschedulable, "select accelerator", message)
}

func preferMode(order []acceleratorv1alpha1.ExecutionMode, preferred acceleratorv1alpha1.ExecutionMode) []acceleratorv1alpha1.ExecutionMode {
	if preferred == "" || len(order) < 2 || order[0] == preferred {
		return order
	}
	for index, mode := range order {
		if mode != preferred {
			continue
		}
		result := append([]acceleratorv1alpha1.ExecutionMode(nil), order...)
		copy(result[1:index+1], result[0:index])
		result[0] = preferred
		return result
	}
	return order
}

func candidateExcluded(candidate contracts.Candidate, exclusions []contracts.CandidateExclusion) bool {
	targetName := candidate.NodeName
	if candidate.Mode == acceleratorv1alpha1.ExecutionModeRemote {
		targetName = candidate.EndpointName
	}
	for _, exclusion := range exclusions {
		if exclusion.Mode == candidate.Mode && exclusion.PoolName == candidate.PoolName && exclusion.TargetName == targetName {
			return true
		}
	}
	return false
}

func placementOrder(placement acceleratorv1alpha1.PlacementPolicy) ([]acceleratorv1alpha1.ExecutionMode, error) {
	switch placement.Mode {
	case "", acceleratorv1alpha1.PlacementModeAuto:
		if len(placement.Order) == 0 {
			return []acceleratorv1alpha1.ExecutionMode{
				acceleratorv1alpha1.ExecutionModeLocal,
				acceleratorv1alpha1.ExecutionModeRemote,
			}, nil
		}
	case acceleratorv1alpha1.PlacementModeLocalOnly:
		return []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeLocal}, nil
	case acceleratorv1alpha1.PlacementModeRemoteOnly:
		return []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeRemote}, nil
	default:
		return nil, contracts.NewError(contracts.ErrorKindInvalid, "normalize placement", fmt.Sprintf("unknown mode %q", placement.Mode))
	}

	seen := map[acceleratorv1alpha1.ExecutionMode]struct{}{}
	order := make([]acceleratorv1alpha1.ExecutionMode, 0, len(placement.Order))
	for _, mode := range placement.Order {
		if mode != acceleratorv1alpha1.ExecutionModeLocal && mode != acceleratorv1alpha1.ExecutionModeRemote {
			return nil, contracts.NewError(contracts.ErrorKindInvalid, "normalize placement", fmt.Sprintf("unknown execution mode %q", mode))
		}
		if _, exists := seen[mode]; exists {
			return nil, contracts.NewError(contracts.ErrorKindInvalid, "normalize placement", fmt.Sprintf("execution mode %q is duplicated", mode))
		}
		seen[mode] = struct{}{}
		order = append(order, mode)
	}
	return order, nil
}

func compatibleDevices(request acceleratorv1alpha1.AcceleratorRequest, candidate contracts.Candidate) ([]contracts.Device, bool) {
	if !candidate.Ready || candidate.DeviceType != request.DeviceType {
		return nil, false
	}
	if !containsLabels(candidate.Labels, request.MatchLabels) {
		return nil, false
	}

	if candidate.Mode == acceleratorv1alpha1.ExecutionModeLocal {
		if !localCapacitySufficient(candidate, request.Count) {
			return nil, false
		}
		if request.Vendor != "" && !strings.EqualFold(request.Vendor, candidate.Vendor) {
			return nil, false
		}
		if request.ResourceForm != "" && request.ResourceForm != candidate.ResourceForm {
			return nil, false
		}
		if request.Profile != "" && !strings.EqualFold(request.Profile, candidate.Profile) {
			return nil, false
		}
		if request.Isolation != "" && !strings.EqualFold(request.Isolation, candidate.Isolation) {
			return nil, false
		}
		if !containsLabels(candidate.Attributes, request.MatchAttributes) || !modelAllowed(candidate.Model, request.Models) {
			return nil, false
		}
		if !request.Memory.IsZero() && (candidate.DeviceMemory.IsZero() || candidate.DeviceMemory.Cmp(request.Memory) < 0) {
			return nil, false
		}
		return nil, true
	}
	if candidate.Available <= 0 {
		return nil, false
	}

	devices := make([]contracts.Device, 0, len(candidate.Devices))
	for _, device := range candidate.Devices {
		if request.Vendor != "" && !strings.EqualFold(request.Vendor, device.Vendor) {
			continue
		}
		if !modelAllowed(device.Model, request.Models) {
			continue
		}
		if !request.Memory.IsZero() && device.Memory.Cmp(request.Memory) < 0 {
			continue
		}
		if request.ResourceForm != "" && request.ResourceForm != device.ResourceForm {
			continue
		}
		if request.Profile != "" && !strings.EqualFold(request.Profile, device.Profile) {
			continue
		}
		if request.Isolation != "" && !strings.EqualFold(request.Isolation, device.Isolation) {
			continue
		}
		if !containsLabels(device.Attributes, request.MatchAttributes) {
			continue
		}
		devices = append(devices, device)
	}
	return devices, len(devices) > 0
}

func selectLocal(count int32, candidates []eligibleCandidate) (contracts.Decision, bool) {
	for _, eligible := range candidates {
		candidate := eligible.candidate
		if candidate.Mode != acceleratorv1alpha1.ExecutionModeLocal || !localCapacitySufficient(candidate, count) {
			continue
		}
		return contracts.Decision{
			ProviderName: candidate.ProviderName,
			PoolName:     candidate.PoolName,
			Mode:         candidate.Mode,
			Targets: []acceleratorv1alpha1.AllocationTarget{{
				NodeName:     candidate.NodeName,
				Count:        count,
				ResourceName: candidate.ResourceName,
				ResourceForm: candidate.ResourceForm,
				Profile:      candidate.Profile,
				Isolation:    candidate.Isolation,
				Attributes:   copyStringMap(candidate.Attributes),
			}},
			Policy: LocalFirstPolicyName,
			Reason: fmt.Sprintf("selected local node %s from pool %s", candidate.NodeName, candidate.PoolName),
		}, true
	}
	return contracts.Decision{}, false
}

func selectRemote(count int32, candidates []eligibleCandidate) (contracts.Decision, bool) {
	for start := 0; start < len(candidates); {
		first := candidates[start].candidate
		if first.Mode != acceleratorv1alpha1.ExecutionModeRemote {
			start++
			continue
		}
		end := start + 1
		for end < len(candidates) &&
			candidates[end].candidate.Mode == first.Mode &&
			candidates[end].candidate.PoolName == first.PoolName &&
			candidates[end].candidate.ProviderName == first.ProviderName {
			end++
		}

		remaining := count
		targets := make([]acceleratorv1alpha1.AllocationTarget, 0, end-start)
		for index := start; index < end; index++ {
			if int32(len(candidates[index].devices)) < count {
				continue
			}
			// A static exclusive runtime exposes every device visible to its
			// server process. Selecting only a subset would let the workload use
			// capacity it did not request, so exact coverage is mandatory.
			if candidates[index].candidate.Exclusive && int32(len(candidates[index].devices)) != count {
				continue
			}
			ids := make([]string, count)
			for deviceIndex := int32(0); deviceIndex < count; deviceIndex++ {
				ids[deviceIndex] = candidates[index].devices[deviceIndex].ID
			}
			return contracts.Decision{
				ProviderName: first.ProviderName,
				PoolName:     first.PoolName,
				Mode:         first.Mode,
				Targets: []acceleratorv1alpha1.AllocationTarget{{
					EndpointName: candidates[index].candidate.EndpointName,
					DeviceIDs:    ids,
					Exclusive:    candidates[index].candidate.Exclusive,
				}},
				Policy: LocalFirstPolicyName,
				Reason: fmt.Sprintf("selected %d remote device(s) from endpoint %s in pool %s", count, candidates[index].candidate.EndpointName, first.PoolName),
			}, true
		}
		for index := start; index < end && remaining > 0; index++ {
			eligible := candidates[index]
			if eligible.candidate.Exclusive {
				continue
			}
			take := int32(len(eligible.devices))
			if take > remaining {
				take = remaining
			}
			if take == 0 {
				continue
			}
			ids := make([]string, take)
			for deviceIndex := int32(0); deviceIndex < take; deviceIndex++ {
				ids[deviceIndex] = eligible.devices[deviceIndex].ID
			}
			targets = append(targets, acceleratorv1alpha1.AllocationTarget{
				EndpointName: eligible.candidate.EndpointName,
				DeviceIDs:    ids,
			})
			remaining -= take
		}
		if remaining == 0 {
			return contracts.Decision{
				ProviderName: first.ProviderName,
				PoolName:     first.PoolName,
				Mode:         first.Mode,
				Targets:      targets,
				Policy:       LocalFirstPolicyName,
				Reason:       fmt.Sprintf("selected %d remote device(s) from pool %s", count, first.PoolName),
			}, true
		}
		start = end
	}
	return contracts.Decision{}, false
}

func freeCount(candidate eligibleCandidate) int32 {
	if candidate.candidate.Mode == acceleratorv1alpha1.ExecutionModeRemote {
		return int32(len(candidate.devices))
	}
	if !candidate.candidate.AvailableQuantity.IsZero() {
		value := candidate.candidate.AvailableQuantity.Value()
		if value > int64(^uint32(0)>>1) {
			return int32(^uint32(0) >> 1)
		}
		return int32(value)
	}
	return candidate.candidate.Available
}

func localCapacitySufficient(candidate contracts.Candidate, count int32) bool {
	if !candidate.AvailableQuantity.IsZero() {
		requested := resource.NewQuantity(int64(count), resource.DecimalSI)
		return candidate.AvailableQuantity.Cmp(*requested) >= 0
	}
	return candidate.Available >= count
}

func modelAllowed(model string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, item := range allowed {
		if strings.EqualFold(item, model) {
			return true
		}
	}
	return false
}

func containsLabels(actual, required map[string]string) bool {
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func copyStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
