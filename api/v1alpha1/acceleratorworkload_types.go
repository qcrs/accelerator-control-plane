package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=Pending;Reserving;Starting;Running;Succeeded;Failed;Terminating
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
	// +kubebuilder:validation:MaxItems=2
	// +listType=set
	Order []ExecutionMode `json:"order,omitempty"`

	PoolSelector *metav1.LabelSelector `json:"poolSelector,omitempty"`

	// +kubebuilder:default="5m"
	ReservationTTL metav1.Duration `json:"reservationTTL,omitempty"`

	// Fallback controls retries that happen only while the generated Pod has
	// never reached Ready. It is intentionally separate from application-level
	// restart or checkpoint recovery.
	Fallback *FallbackPolicy `json:"fallback,omitempty"`
}

type FallbackPolicy struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// MaxAttempts includes the initial startup attempt.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=8
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// +kubebuilder:default="5m"
	StartupTimeout metav1.Duration `json:"startupTimeout,omitempty"`

	// +kubebuilder:default="5s"
	Backoff metav1.Duration `json:"backoff,omitempty"`
}

type AcceleratorWorkloadSpec struct {
	Accelerator AcceleratorRequest `json:"accelerator"`
	Placement   PlacementPolicy    `json:"placement,omitempty"`
	// The CRD overlay expands the safe PodTemplate ObjectMeta fields because
	// controller-gen intentionally leaves external ObjectMeta schemas opaque.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
}

type AcceleratorWorkloadStatus struct {
	ObservedGeneration int64         `json:"observedGeneration,omitempty"`
	Phase              WorkloadPhase `json:"phase,omitempty"`

	SelectedMode      ExecutionMode           `json:"selectedMode,omitempty"`
	AllocationRef     string                  `json:"allocationRef,omitempty"`
	AllocationAttempt int32                   `json:"allocationAttempt,omitempty"`
	PodRef            *corev1.ObjectReference `json:"podRef,omitempty"`

	Decision *DecisionStatus `json:"decision,omitempty"`

	Fallback FallbackStatus `json:"fallback,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FallbackStatus is durable so controller restarts do not reset retry limits,
// retry deadlines, or failed-target exclusions.
type FallbackStatus struct {
	FailedAttempts int32 `json:"failedAttempts,omitempty"`
	// ReadyObserved is monotonic. Once true, this Workload can never enter a
	// pre-Ready fallback path again.
	ReadyObserved bool `json:"readyObserved,omitempty"`

	RequeueAt        *metav1.Time `json:"requeueAt,omitempty"`
	StartupStartedAt *metav1.Time `json:"startupStartedAt,omitempty"`

	// +kubebuilder:validation:MaxItems=8
	History []StartupFailureStatus `json:"history,omitempty"`
}

type StartupFailureStatus struct {
	// Attempt is the allocation attempt whose Pod failed to become Ready.
	Attempt int32 `json:"attempt"`

	Mode        ExecutionMode          `json:"mode"`
	ProviderRef ClusterObjectReference `json:"providerRef"`
	PoolRef     ClusterObjectReference `json:"poolRef"`
	Targets     []AllocationTarget     `json:"targets"`

	PodRef *corev1.ObjectReference `json:"podRef,omitempty"`

	StartedAt metav1.Time `json:"startedAt"`
	FailedAt  metav1.Time `json:"failedAt"`

	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aw
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.allocationAttempt) || oldSelf.status.allocationAttempt == 0 || self.spec == oldSelf.spec",message="spec is immutable after allocation starts"
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
