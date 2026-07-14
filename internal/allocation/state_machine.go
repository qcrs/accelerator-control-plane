package allocation

import (
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
)

var allowedTransitions = map[acceleratorv1alpha1.AllocationPhase]map[acceleratorv1alpha1.AllocationPhase]struct{}{
	"": {
		acceleratorv1alpha1.AllocationPhasePending:   {},
		acceleratorv1alpha1.AllocationPhaseReleasing: {},
	},
	acceleratorv1alpha1.AllocationPhasePending: {
		acceleratorv1alpha1.AllocationPhaseReserved:  {},
		acceleratorv1alpha1.AllocationPhaseReleasing: {},
		acceleratorv1alpha1.AllocationPhaseFailed:    {},
		acceleratorv1alpha1.AllocationPhaseExpired:   {},
	},
	acceleratorv1alpha1.AllocationPhaseReserved: {
		acceleratorv1alpha1.AllocationPhaseBound:     {},
		acceleratorv1alpha1.AllocationPhaseReleasing: {},
		acceleratorv1alpha1.AllocationPhaseFailed:    {},
		acceleratorv1alpha1.AllocationPhaseExpired:   {},
	},
	acceleratorv1alpha1.AllocationPhaseBound: {
		acceleratorv1alpha1.AllocationPhaseReleasing: {},
		acceleratorv1alpha1.AllocationPhaseFailed:    {},
	},
	acceleratorv1alpha1.AllocationPhaseReleasing: {
		acceleratorv1alpha1.AllocationPhaseReleased: {},
		acceleratorv1alpha1.AllocationPhaseFailed:   {},
	},
}

func CanTransition(from, to acceleratorv1alpha1.AllocationPhase) bool {
	if from == to {
		return true
	}
	_, allowed := allowedTransitions[from][to]
	return allowed
}

func Transition(lease *acceleratorv1alpha1.AcceleratorLease, to acceleratorv1alpha1.AllocationPhase) error {
	if lease == nil {
		return fmt.Errorf("lease is nil")
	}
	if !CanTransition(lease.Status.Phase, to) {
		return fmt.Errorf("invalid allocation transition %q -> %q", lease.Status.Phase, to)
	}
	lease.Status.Phase = to
	return nil
}

func IsTerminal(phase acceleratorv1alpha1.AllocationPhase) bool {
	switch phase {
	case acceleratorv1alpha1.AllocationPhaseReleased,
		acceleratorv1alpha1.AllocationPhaseFailed,
		acceleratorv1alpha1.AllocationPhaseExpired:
		return true
	default:
		return false
	}
}

func IsExpired(lease *acceleratorv1alpha1.AcceleratorLease, now time.Time) bool {
	if lease == nil || lease.Status.ExpiresAt == nil {
		return false
	}
	if lease.Status.Phase != acceleratorv1alpha1.AllocationPhasePending &&
		lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseReserved {
		return false
	}
	return !now.Before(lease.Status.ExpiresAt.Time)
}
