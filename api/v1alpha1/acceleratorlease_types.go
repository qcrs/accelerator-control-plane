package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type AllocationPhase string

const (
	AllocationPhasePending   AllocationPhase = "Pending"
	AllocationPhaseReserved  AllocationPhase = "Reserved"
	AllocationPhaseBound     AllocationPhase = "Bound"
	AllocationPhaseReleasing AllocationPhase = "Releasing"
	AllocationPhaseReleased  AllocationPhase = "Released"
	AllocationPhaseFailed    AllocationPhase = "Failed"
	AllocationPhaseExpired   AllocationPhase = "Expired"
)

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type AcceleratorLeaseSpec struct {
	Owner corev1.ObjectReference `json:"owner"`

	// WorkloadGeneration pins the exact Workload spec version that was used
	// for selection. It prevents a retry from attaching an old Lease to a
	// Workload whose spec changed before status was persisted.
	// +kubebuilder:validation:Minimum=1
	WorkloadGeneration int64 `json:"workloadGeneration"`

	Request AcceleratorRequest `json:"request"`

	ProviderRef ClusterObjectReference `json:"providerRef"`
	PoolRef     ClusterObjectReference `json:"poolRef"`

	// +kubebuilder:validation:Enum=Local;Remote
	Mode ExecutionMode `json:"mode"`

	// +kubebuilder:validation:MinItems=1
	Targets []AllocationTarget `json:"targets"`

	TTL metav1.Duration `json:"ttl"`
}

type AcceleratorLeaseStatus struct {
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	Phase              AllocationPhase `json:"phase,omitempty"`

	Binding   RuntimeBinding          `json:"binding,omitempty"`
	ExpiresAt *metav1.Time            `json:"expiresAt,omitempty"`
	PodRef    *corev1.ObjectReference `json:"podRef,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=alease
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
type AcceleratorLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AcceleratorLeaseSpec   `json:"spec,omitempty"`
	Status AcceleratorLeaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AcceleratorLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AcceleratorLease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AcceleratorLease{}, &AcceleratorLeaseList{})
}
