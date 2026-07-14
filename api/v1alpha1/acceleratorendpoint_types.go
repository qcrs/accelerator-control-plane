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
	Driver     string `json:"driver,omitempty"`
	Version    string `json:"version,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
}

type DeviceInfo struct {
	ID     string `json:"id"`
	UUID   string `json:"uuid,omitempty"`
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`

	Memory resource.Quantity `json:"memory,omitempty"`

	// +kubebuilder:validation:Enum=Healthy;Unhealthy;Unknown
	Health DeviceHealth `json:"health"`

	Attributes map[string]string `json:"attributes,omitempty"`
}

type AcceleratorEndpointSpec struct {
	PoolRef ClusterObjectReference `json:"poolRef"`

	Unschedulable bool `json:"unschedulable,omitempty"`
}

type AcceleratorEndpointStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	AgentID        string `json:"agentID,omitempty"`
	ControlAddress string `json:"controlAddress,omitempty"`

	Runtime RuntimeInfo `json:"runtime,omitempty"`

	HeartbeatLeaseRef *HeartbeatLeaseReference `json:"heartbeatLeaseRef,omitempty"`
	LastInventoryTime *metav1.Time             `json:"lastInventoryTime,omitempty"`

	Devices    []DeviceInfo       `json:"devices,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=aendpoint
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="AgentReady",type=string,JSONPath=`.status.conditions[?(@.type=="AgentReady")].status`
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
