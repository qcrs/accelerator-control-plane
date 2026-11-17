package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type LocalPoolConfig struct {
	ResourceName corev1.ResourceName `json:"resourceName"`

	// +kubebuilder:default=Custom
	// +kubebuilder:validation:Enum=WholeDevice;MIG;Shared;Custom
	ResourceForm ResourceForm `json:"resourceForm,omitempty"`

	Profile    string            `json:"profile,omitempty"`
	Isolation  string            `json:"isolation,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`

	Vendor string            `json:"vendor,omitempty"`
	Model  string            `json:"model,omitempty"`
	Memory resource.Quantity `json:"memory,omitempty"`

	NodeSelector   map[string]string `json:"nodeSelector,omitempty"`
	VendorLabelKey string            `json:"vendorLabelKey,omitempty"`
	ModelLabelKey  string            `json:"modelLabelKey,omitempty"`
	MemoryLabelKey string            `json:"memoryLabelKey,omitempty"`
}

type RemotePoolConfig struct {
	EndpointSelector metav1.LabelSelector `json:"endpointSelector,omitempty"`

	// +kubebuilder:default=ExclusiveEndpoint
	// +kubebuilder:validation:Enum=ExclusiveEndpoint;Device
	AllocationMode RemoteAllocationMode `json:"allocationMode,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.location == 'Local' ? has(self.local) && !has(self.remote) : has(self.remote) && !has(self.local)",message="exactly one location-specific configuration must be set"
type AcceleratorPoolSpec struct {
	ProviderRef ClusterObjectReference `json:"providerRef"`

	// +kubebuilder:validation:Enum=Local;Remote
	Location ExecutionMode `json:"location"`

	// +kubebuilder:validation:Enum=GPU;NPU
	DeviceType DeviceKind `json:"deviceType"`

	Priority int32 `json:"priority,omitempty"`

	Unschedulable bool `json:"unschedulable,omitempty"`

	Local  *LocalPoolConfig  `json:"local,omitempty"`
	Remote *RemotePoolConfig `json:"remote,omitempty"`
}

type CapacitySummary struct {
	Total     int32 `json:"total,omitempty"`
	Available int32 `json:"available,omitempty"`
	Reserved  int32 `json:"reserved,omitempty"`
}

type AcceleratorPoolStatus struct {
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	Capacity           CapacitySummary `json:"capacity,omitempty"`
	ReadyEndpoints     int32           `json:"readyEndpoints,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=apool
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.location`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.deviceType`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.capacity.available`
type AcceleratorPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AcceleratorPoolSpec   `json:"spec,omitempty"`
	Status AcceleratorPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AcceleratorPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AcceleratorPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AcceleratorPool{}, &AcceleratorPoolList{})
}
