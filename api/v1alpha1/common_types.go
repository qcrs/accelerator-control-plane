package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=GPU;NPU
type DeviceKind string

const (
	DeviceKindGPU DeviceKind = "GPU"
	DeviceKindNPU DeviceKind = "NPU"
)

// +kubebuilder:validation:Enum=WholeDevice;MIG;Shared;Custom
type ResourceForm string

const (
	ResourceFormWholeDevice ResourceForm = "WholeDevice"
	ResourceFormMIG         ResourceForm = "MIG"
	ResourceFormShared      ResourceForm = "Shared"
	ResourceFormCustom      ResourceForm = "Custom"
)

// +kubebuilder:validation:Enum=Local;Remote
type ExecutionMode string

const (
	ExecutionModeLocal  ExecutionMode = "Local"
	ExecutionModeRemote ExecutionMode = "Remote"
)

// +kubebuilder:validation:Enum=Auto;LocalOnly;RemoteOnly
type PlacementMode string

const (
	PlacementModeAuto       PlacementMode = "Auto"
	PlacementModeLocalOnly  PlacementMode = "LocalOnly"
	PlacementModeRemoteOnly PlacementMode = "RemoteOnly"
)

// +kubebuilder:validation:Enum=ExclusiveEndpoint;Device
type RemoteAllocationMode string

const (
	// RemoteAllocationModeExclusiveEndpoint reserves the complete remote
	// runtime endpoint. Static runtimes must expose exactly the selected devices.
	RemoteAllocationModeExclusiveEndpoint RemoteAllocationMode = "ExclusiveEndpoint"
	// RemoteAllocationModeDevice allows independent devices on an endpoint to
	// be assigned to different workloads. Providers must explicitly support it.
	RemoteAllocationModeDevice RemoteAllocationMode = "Device"
)

// ClusterObjectReference identifies a cluster-scoped API object by name.
type ClusterObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// NamespacedObjectReference identifies a namespaced API object.
type NamespacedObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type AcceleratorRequest struct {
	// +kubebuilder:validation:Enum=GPU;NPU
	DeviceType DeviceKind `json:"deviceType"`

	Vendor string `json:"vendor,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	Count int32 `json:"count"`

	Memory resource.Quantity `json:"memory,omitempty"`

	// ResourceForm, Profile, and Isolation distinguish whole devices from
	// hardware partitions and software-defined resource units. Empty values do
	// not constrain placement.
	ResourceForm ResourceForm `json:"resourceForm,omitempty"`

	Profile string `json:"profile,omitempty"`

	Isolation string `json:"isolation,omitempty"`

	Models []string `json:"models,omitempty"`

	MatchLabels map[string]string `json:"matchLabels,omitempty"`

	MatchAttributes map[string]string `json:"matchAttributes,omitempty"`

	// ContainerName is the only container that receives accelerator resources.
	// It defaults to the first regular container in the pod template.
	ContainerName string `json:"containerName,omitempty"`
}

type AllocationTarget struct {
	NodeName     string   `json:"nodeName,omitempty"`
	EndpointName string   `json:"endpointName,omitempty"`
	DeviceIDs    []string `json:"deviceIDs,omitempty"`
	Exclusive    bool     `json:"exclusive,omitempty"`

	// Count is the number of units advertised under ResourceName. A unit may be
	// a whole device, a MIG instance, or a software-defined share; it is not
	// assumed to be a physical card.
	Count int32 `json:"count,omitempty"`

	// Local resource metadata is copied into the immutable Lease spec so Pool
	// edits or deletion cannot redirect injection or make reservations vanish
	// from accounting.
	ResourceName corev1.ResourceName `json:"resourceName,omitempty"`
	ResourceForm ResourceForm        `json:"resourceForm,omitempty"`
	Profile      string              `json:"profile,omitempty"`
	Isolation    string              `json:"isolation,omitempty"`
	Attributes   map[string]string   `json:"attributes,omitempty"`
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

type BoundEndpoint struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Address    string            `json:"address"`
	Runtime    RuntimeInfo       `json:"runtime"`
	Parameters map[string]string `json:"parameters,omitempty"`
}

type RuntimeBinding struct {
	ID string `json:"id,omitempty"`

	// +listType=map
	// +listMapKey=name
	Endpoints []BoundEndpoint `json:"endpoints,omitempty"`
}
