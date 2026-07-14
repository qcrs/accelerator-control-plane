package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ClientRuntimeSpec struct {
	Image string `json:"image"`

	// SourcePath is copied from the client image into a shared volume.
	SourcePath string `json:"sourcePath,omitempty"`

	// MountPath is where the shared runtime is mounted in the application.
	MountPath string `json:"mountPath,omitempty"`
}

type AcceleratorProviderSpec struct {
	Driver string `json:"driver"`

	// +kubebuilder:validation:MinItems=1
	DeviceKinds []DeviceKind `json:"deviceKinds"`

	ClientRuntime *ClientRuntimeSpec `json:"clientRuntime,omitempty"`

	TrustBundleSecretRef *corev1.SecretReference `json:"trustBundleSecretRef,omitempty"`

	// Parameters contains driver-specific configuration.
	// +kubebuilder:pruning:PreserveUnknownFields
	Parameters runtime.RawExtension `json:"parameters,omitempty"`
}

type AcceleratorProviderStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=aprovider
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.spec.driver`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type AcceleratorProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AcceleratorProviderSpec   `json:"spec,omitempty"`
	Status AcceleratorProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AcceleratorProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AcceleratorProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AcceleratorProvider{}, &AcceleratorProviderList{})
}
