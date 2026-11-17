package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DeviceHealth string

const (
	DeviceHealthHealthy   DeviceHealth = "Healthy"
	DeviceHealthUnhealthy DeviceHealth = "Unhealthy"
	DeviceHealthUnknown   DeviceHealth = "Unknown"
)

type RuntimeInfo struct {
	// +kubebuilder:validation:MinLength=1
	Driver     string `json:"driver"`
	Version    string `json:"version,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
}

type DeviceInfo struct {
	// +kubebuilder:validation:MinLength=1
	ID     string `json:"id"`
	UUID   string `json:"uuid,omitempty"`
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`

	Memory resource.Quantity `json:"memory,omitempty"`

	// +kubebuilder:default=WholeDevice
	// +kubebuilder:validation:Enum=WholeDevice;MIG;Shared;Custom
	ResourceForm ResourceForm `json:"resourceForm,omitempty"`
	Profile      string       `json:"profile,omitempty"`
	Isolation    string       `json:"isolation,omitempty"`

	// +kubebuilder:validation:Enum=Healthy;Unhealthy;Unknown
	Health DeviceHealth `json:"health"`

	Attributes map[string]string `json:"attributes,omitempty"`
}

type AcceleratorEndpointSpec struct {
	PoolRef ClusterObjectReference `json:"poolRef"`

	// Address is the provider-specific remote runtime address. It is copied
	// into each allocation binding so later edits do not retarget running Pods.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Address string `json:"address"`

	Runtime RuntimeInfo `json:"runtime"`

	// Devices is the capacity exposed by this runtime endpoint. For a static
	// Cricket deployment it must match CUDA_VISIBLE_DEVICES on the server.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=id
	Devices []DeviceInfo `json:"devices"`

	// Parameters contains non-secret provider-specific connection values.
	Parameters map[string]string `json:"parameters,omitempty"`

	Unschedulable bool `json:"unschedulable,omitempty"`
}

type AcceleratorEndpointStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	LastProbeTime     *metav1.Time    `json:"lastProbeTime,omitempty"`
	Capacity          CapacitySummary `json:"capacity,omitempty"`
	Health            DeviceHealth    `json:"health,omitempty"`
	Reserved          int32           `json:"reserved,omitempty"`
	Available         int32           `json:"available,omitempty"`
	ActiveSessions    *int32          `json:"activeSessions,omitempty"`
	ObservedAt        *metav1.Time    `json:"observedAt,omitempty"`
	ObservationSource string          `json:"observationSource,omitempty"`
	Confidence        string          `json:"confidence,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	Message           string          `json:"message,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=aendpoint
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="Configured",type=string,JSONPath=`.status.conditions[?(@.type=="Configured")].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type AcceleratorEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AcceleratorEndpointSpec   `json:"spec,omitempty"`
	Status AcceleratorEndpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AcceleratorEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AcceleratorEndpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AcceleratorEndpoint{}, &AcceleratorEndpointList{})
}
