package contracts

import (
	"context"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type CapabilitySet struct {
	DeviceKinds  []acceleratorv1alpha1.DeviceKind
	Modes        []acceleratorv1alpha1.ExecutionMode
	SupportsTLS  bool
	SupportsAuth bool
}

type Device struct {
	ID         string
	Vendor     string
	Model      string
	Memory     resource.Quantity
	Attributes map[string]string
}

// Candidate is a normalized allocation unit. Local candidates represent one
// Kubernetes Node; remote candidates represent one Agent endpoint.
type Candidate struct {
	ID           string
	ProviderName string
	PoolName     string
	PoolPriority int32
	Mode         acceleratorv1alpha1.ExecutionMode
	DeviceType   acceleratorv1alpha1.DeviceKind

	NodeName       string
	EndpointName   string
	ControlAddress string
	ResourceName   corev1.ResourceName

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
	Endpoints []string
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

type AgentReservationRequest struct {
	LeaseKey    string
	DeviceIDs   []string
	ExpiresAt   time.Time
	WorkloadUID string
}

type AgentReservation struct {
	BindingID string
	Endpoints []string
}

type AgentClient interface {
	Reserve(ctx context.Context, controlAddress string, request AgentReservationRequest) (AgentReservation, error)
	Release(ctx context.Context, controlAddress, leaseKey string) error
}

type EndpointResolver interface {
	ControlAddress(ctx context.Context, endpointName string) (string, error)
}

type DeviceProbe interface {
	Probe(ctx context.Context) ([]acceleratorv1alpha1.DeviceInfo, acceleratorv1alpha1.RuntimeInfo, error)
}

type SessionProvisioner interface {
	Prepare(ctx context.Context, request AgentReservationRequest) (AgentReservation, error)
	Release(ctx context.Context, leaseKey string) error
}
