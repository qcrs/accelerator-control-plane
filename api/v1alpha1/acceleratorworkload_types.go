package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WorkloadPhase string

const (
	WorkloadPhasePending     WorkloadPhase = "Pending"
	WorkloadPhaseReserving   WorkloadPhase = "Reserving"
	WorkloadPhaseStarting    WorkloadPhase = "Starting"
	WorkloadPhaseRunning     WorkloadPhase = "Running"
	WorkloadPhaseSucceeded   WorkloadPhase = "Succeeded"
	WorkloadPhaseFailed      WorkloadPhase = "Failed"
	WorkloadPhaseTerminating WorkloadPhase = "Terminating"
)

type PlacementPolicy struct {
	// +kubebuilder:default=Auto
	// +kubebuilder:validation:Enum=Auto;LocalOnly;RemoteOnly
	Mode PlacementMode `json:"mode,omitempty"`

	// Order is used by Auto mode and defaults to Local, Remote.
	Order []ExecutionMode `json:"order,omitempty"`

	PoolSelector *metav1.LabelSelector `json:"poolSelector,omitempty"`

	// +kubebuilder:default="5m"
	ReservationTTL metav1.Duration `json:"reservationTTL,omitempty"`
}

type AcceleratorWorkloadSpec struct {
	Accelerator AcceleratorRequest     `json:"accelerator"`
	Placement   PlacementPolicy        `json:"placement,omitempty"`
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
}

type AcceleratorWorkloadStatus struct {
	ObservedGeneration int64         `json:"observedGeneration,omitempty"`
	Phase              WorkloadPhase `json:"phase,omitempty"`

	SelectedMode      ExecutionMode           `json:"selectedMode,omitempty"`
	AllocationRef     string                  `json:"allocationRef,omitempty"`
	AllocationAttempt int32                   `json:"allocationAttempt,omitempty"`
	PodRef            *corev1.ObjectReference `json:"podRef,omitempty"`

	Decision   *DecisionStatus    `json:"decision,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aw
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.allocationRef) || size(oldSelf.status.allocationRef) == 0 || self.spec == oldSelf.spec",message="spec is immutable after allocation starts"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.status.selectedMode`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podRef.name`
type AcceleratorWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AcceleratorWorkloadSpec   `json:"spec,omitempty"`
	Status AcceleratorWorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AcceleratorWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AcceleratorWorkload `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AcceleratorWorkload{}, &AcceleratorWorkloadList{})
}
