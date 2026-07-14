package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DeviceKind string

const (
	DeviceKindGPU DeviceKind = "GPU"
	DeviceKindNPU DeviceKind = "NPU"
)

type ExecutionMode string

const (
	ExecutionModeLocal  ExecutionMode = "Local"
	ExecutionModeRemote ExecutionMode = "Remote"
)

type PlacementMode string

const (
	PlacementModeAuto       PlacementMode = "Auto"
	PlacementModeLocalOnly  PlacementMode = "LocalOnly"
	PlacementModeRemoteOnly PlacementMode = "RemoteOnly"
)

// ClusterObjectReference identifies a cluster-scoped API object by name.
type ClusterObjectReference struct {
	Name string `json:"name"`
}

// NamespacedObjectReference identifies a namespaced API object.
type NamespacedObjectReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type HeartbeatLeaseReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type AcceleratorRequest struct {
	// +kubebuilder:validation:Enum=GPU;NPU
	DeviceType DeviceKind `json:"deviceType"`

	Vendor string `json:"vendor,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	Count int32 `json:"count"`

	Memory resource.Quantity `json:"memory,omitempty"`

	Models []string `json:"models,omitempty"`

	MatchLabels map[string]string `json:"matchLabels,omitempty"`

	// ContainerName is the only container that receives accelerator resources.
	// It defaults to the first regular container in the pod template.
	ContainerName string `json:"containerName,omitempty"`
}

type AllocationTarget struct {
	NodeName     string   `json:"nodeName,omitempty"`
	EndpointName string   `json:"endpointName,omitempty"`
	DeviceIDs    []string `json:"deviceIDs,omitempty"`

	// Count is used for local extended resources where the device plugin owns
	// concrete device selection.
	Count int32 `json:"count,omitempty"`
}

type DecisionStatus struct {
	Mode ExecutionMode `json:"mode,omitempty"`

	ProviderRef ClusterObjectReference `json:"providerRef,omitempty"`
	PoolRef     ClusterObjectReference `json:"poolRef,omitempty"`
	Targets     []AllocationTarget     `json:"targets,omitempty"`

	Policy    string      `json:"policy,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	DecidedAt metav1.Time `json:"decidedAt,omitempty"`
}

type RuntimeBinding struct {
	ID        string   `json:"id,omitempty"`
	Endpoints []string `json:"endpoints,omitempty"`
}
