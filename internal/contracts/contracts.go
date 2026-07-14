package contracts

import (
	"context"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type CapabilitySet struct {
	DeviceKinds           []acceleratorv1alpha1.DeviceKind
	Modes                 []acceleratorv1alpha1.ExecutionMode
	RemoteAllocationModes []acceleratorv1alpha1.RemoteAllocationMode
	SupportsTLS           bool
	SupportsAuth          bool
}

type Device struct {
	ID         string
	Vendor     string
	Model      string
	Memory     resource.Quantity
	Attributes map[string]string
}

// Candidate is a normalized allocation unit. Local candidates represent one
// Kubernetes Node; remote candidates represent one runtime endpoint.
type Candidate struct {
	ID           string
	ProviderName string
	PoolName     string
	PoolPriority int32
	Mode         acceleratorv1alpha1.ExecutionMode
	DeviceType   acceleratorv1alpha1.DeviceKind

	NodeName       string
	EndpointName   string
	ResourceName   corev1.ResourceName
	Exclusive      bool

	Vendor       string
	Model        string
	DeviceMemory resource.Quantity
	Labels       map[string]string
	Available    int32
	Devices      []Device
	Ready        bool
	Reason       string
}

type SelectionRequest struct {
	Accelerator acceleratorv1alpha1.AcceleratorRequest
	Placement   acceleratorv1alpha1.PlacementPolicy
	PodTemplate corev1.PodTemplateSpec
}

type Decision struct {
	ProviderName string
	PoolName     string
	Mode         acceleratorv1alpha1.ExecutionMode
	Targets      []acceleratorv1alpha1.AllocationTarget
	Policy       string
	Reason       string
}

type InventoryReader interface {
	ListCandidates(ctx context.Context, request SelectionRequest) ([]Candidate, error)
}

type PolicyEngine interface {
	Select(ctx context.Context, request SelectionRequest, candidates []Candidate) (Decision, error)
}

type ReservationRequest struct {
	LeaseKey     string
	WorkloadUID  string
	ProviderName string
	PoolName     string
	Mode         acceleratorv1alpha1.ExecutionMode
	Request      acceleratorv1alpha1.AcceleratorRequest
	Targets      []acceleratorv1alpha1.AllocationTarget
	TTL          time.Duration
}

type Reservation struct {
	BindingID string
	Endpoints []acceleratorv1alpha1.BoundEndpoint
	ExpiresAt time.Time
}

// Allocator operations must be idempotent for the same LeaseKey. A different
// LeaseKey is a different allocation even when all other fields are equal.
type Allocator interface {
	Reserve(ctx context.Context, request ReservationRequest) (Reservation, error)
	Bind(ctx context.Context, request ReservationRequest, pod corev1.ObjectReference) error
	Release(ctx context.Context, request ReservationRequest) error
}

type RuntimePlan struct {
	ContainerName string

	InitContainers []corev1.Container
	Volumes        []corev1.Volume
	Env            []corev1.EnvVar
	VolumeMounts   []corev1.VolumeMount

	Resources        *corev1.ResourceRequirements
	RequiredNodeName string
	Annotations      map[string]string
}

type Provider interface {
	Driver() string
	Capabilities() CapabilitySet
	Validate(provider *acceleratorv1alpha1.AcceleratorProvider, pool *acceleratorv1alpha1.AcceleratorPool) error
	BuildRuntimePlan(
		ctx context.Context,
		provider *acceleratorv1alpha1.AcceleratorProvider,
		pool *acceleratorv1alpha1.AcceleratorPool,
		lease *acceleratorv1alpha1.AcceleratorLease,
	) (RuntimePlan, error)
}

type PodInjector interface {
	Mutate(template corev1.PodTemplateSpec, plan RuntimePlan) (corev1.PodTemplateSpec, error)
	JSONPatch(before, after corev1.PodTemplateSpec) ([]byte, error)
}

type RemoteEndpoint struct {
	Name          string
	PoolName      string
	Address       string
	Runtime       acceleratorv1alpha1.RuntimeInfo
	Devices       []acceleratorv1alpha1.DeviceInfo
	Parameters    map[string]string
	Unschedulable bool
	Readiness     *EndpointReadiness
}

// EndpointReadiness is nil when a static endpoint has no external health
// observer. A non-nil value represents the explicitly published Ready state.
type EndpointReadiness struct {
	Ready   bool
	Reason  string
	Message string
}

type EndpointResolver interface {
	Resolve(ctx context.Context, endpointName string) (RemoteEndpoint, error)
}

// RemoteEndpointValidator owns runtime-specific validation while keeping the
// inventory and allocation layers independent of a concrete remote runtime.
type RemoteEndpointValidator interface {
	ValidateEndpoint(endpoint RemoteEndpoint) error
}
