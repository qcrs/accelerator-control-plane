// Package corex provides the ARM64 CoreX remote-runtime adapter.
package corex

import (
	"context"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

const (
	DriverName        = "corex-remote"
	defaultSourcePath = "/opt/corex-remote/lib"
	defaultMountPath  = "/opt/accelerator/corex"
	defaultPort       = 50051
	parameterPort     = "port"
	runtimeInitName   = "accelerator-runtime-setup"
	runtimeVolume     = "accelerator-runtime"
	runtimeCopyPath   = "/runtime"
)

type Provider struct{}

func New() *Provider { return &Provider{} }

func (p *Provider) Driver() string { return DriverName }

func (p *Provider) Capabilities() contracts.CapabilitySet {
	return contracts.CapabilitySet{
		DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
		Modes:       []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeRemote},
		ResourceForms: []acceleratorv1alpha1.ResourceForm{
			acceleratorv1alpha1.ResourceFormWholeDevice,
		},
		RemoteAllocationModes: []acceleratorv1alpha1.RemoteAllocationMode{
			acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint,
		},
	}
}

func (p *Provider) Validate(provider *acceleratorv1alpha1.AcceleratorProvider, pool *acceleratorv1alpha1.AcceleratorPool) error {
	if provider == nil || pool == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX provider", "provider and pool are required")
	}
	if err := p.ValidateProvider(provider); err != nil {
		return err
	}
	if pool.Spec.Location != acceleratorv1alpha1.ExecutionModeRemote || pool.Spec.Remote == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX provider", "a Remote pool is required")
	}
	if pool.Spec.DeviceType != acceleratorv1alpha1.DeviceKindGPU {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate CoreX provider", "CoreX remote requires a GPU pool")
	}
	if remoteAllocationMode(pool.Spec.Remote) != acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate CoreX provider", "CoreX remote currently requires ExclusiveEndpoint allocation")
	}
	return nil
}

func (p *Provider) ValidateProvider(provider *acceleratorv1alpha1.AcceleratorProvider) error {
	if provider == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX provider", "provider is required")
	}
	if provider.Spec.Driver != DriverName {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX provider", fmt.Sprintf("driver must be %q", DriverName))
	}
	if provider.Spec.ClientRuntime == nil || strings.TrimSpace(provider.Spec.ClientRuntime.Image) == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX provider", "clientRuntime.image is required")
	}
	if strings.TrimSpace(provider.Spec.ClientRuntime.RuntimeVersion) == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX provider", "clientRuntime.runtimeVersion is required")
	}
	if _, _, err := runtimePaths(provider.Spec.ClientRuntime); err != nil {
		return err
	}
	if provider.Spec.TrustBundleSecretRef != nil {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate CoreX provider", "the current CoreX transport does not provide TLS")
	}
	if provider.Spec.AuthSecretRef != nil {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate CoreX provider", "the current CoreX transport does not provide authentication")
	}
	return nil
}

func (p *Provider) BuildRuntimePlan(
	_ context.Context,
	provider *acceleratorv1alpha1.AcceleratorProvider,
	pool *acceleratorv1alpha1.AcceleratorPool,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (contracts.RuntimePlan, error) {
	if err := p.Validate(provider, pool); err != nil {
		return contracts.RuntimePlan{}, err
	}
	if lease == nil || lease.Spec.Mode != acceleratorv1alpha1.ExecutionModeRemote {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build CoreX runtime", "a Remote accelerator lease is required")
	}
	if len(lease.Spec.Targets) != 1 || !lease.Spec.Targets[0].Exclusive {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindUnsupported, "build CoreX runtime", "CoreX remote requires exactly one exclusive endpoint target")
	}
	if lease.Status.Binding.ID == "" || len(lease.Status.Binding.Endpoints) != 1 {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build CoreX runtime", "the lease requires one resolved endpoint binding")
	}
	binding := lease.Status.Binding.Endpoints[0]
	if binding.Name == "" || binding.Name != lease.Spec.Targets[0].EndpointName {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build CoreX runtime", "the endpoint binding does not match the allocation target")
	}
	if binding.Runtime.Version == "" || binding.Runtime.Version != provider.Spec.ClientRuntime.RuntimeVersion {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindUnsupported, "build CoreX runtime", fmt.Sprintf("endpoint runtime version %q is incompatible with client runtime version %q", binding.Runtime.Version, provider.Spec.ClientRuntime.RuntimeVersion))
	}
	host, port, err := endpointConnection(binding.Address, binding.Parameters)
	if err != nil {
		return contracts.RuntimePlan{}, err
	}
	// CoreX CRX9 sessions are sensitive to the Kubernetes service path. Keep
	// the stable StatefulSet DNS name in the Endpoint, but inject a resolved
	// Pod IP into the client so the session bypasses ClusterIP/NAT and remains
	// a direct TCP connection.
	if strings.HasSuffix(host, ".svc.cluster.local") {
		resolved, lookupErr := net.LookupHost(host)
		if lookupErr != nil || len(resolved) == 0 {
			return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindTransient, "resolve CoreX endpoint", fmt.Sprintf("resolve %q: %v", host, lookupErr))
		}
		for _, candidate := range resolved {
			if parsed := net.ParseIP(candidate); parsed != nil && parsed.To4() != nil {
				host = parsed.String()
				break
			}
		}
	}
	sourcePath, mountPath, err := runtimePaths(provider.Spec.ClientRuntime)
	if err != nil {
		return contracts.RuntimePlan{}, err
	}

	return contracts.RuntimePlan{
		Mode:          acceleratorv1alpha1.ExecutionModeRemote,
		ContainerName: lease.Spec.Request.ContainerName,
		Volumes: []corev1.Volume{{
			Name:         runtimeVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
		InitContainers: []corev1.Container{{
			Name:         runtimeInitName,
			Image:        provider.Spec.ClientRuntime.Image,
			Command:      []string{"/bin/cp"},
			Args:         []string{"-a", strings.TrimSuffix(sourcePath, "/") + "/.", runtimeCopyPath + "/"},
			VolumeMounts: []corev1.VolumeMount{{Name: runtimeVolume, MountPath: runtimeCopyPath}},
		}},
		Env: []corev1.EnvVar{
			{Name: "COREX_REMOTE_HOST", Value: host},
			{Name: "COREX_REMOTE_PORT", Value: port},
			{Name: "LD_LIBRARY_PATH", Value: strings.Join([]string{mountPath}, ":")},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: runtimeVolume, MountPath: mountPath, ReadOnly: true}},
		Annotations: map[string]string{
			contracts.RuntimeInitContainersAnnotation: runtimeInitName,
			"accelerator.qcrs.io/mode":                string(acceleratorv1alpha1.ExecutionModeRemote),
			"accelerator.qcrs.io/provider":            lease.Spec.ProviderRef.Name,
			"accelerator.qcrs.io/pool":                lease.Spec.PoolRef.Name,
			"accelerator.qcrs.io/lease":               lease.Name,
			"accelerator.qcrs.io/endpoint":            binding.Name,
		},
	}, nil
}

func (p *Provider) ValidateEndpoint(endpoint contracts.RemoteEndpoint) error {
	if endpoint.Runtime.Driver != DriverName {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX endpoint", fmt.Sprintf("runtime driver must be %q", DriverName))
	}
	if strings.TrimSpace(endpoint.Runtime.Version) == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX endpoint", "runtime.version is required")
	}
	_, _, err := endpointConnection(endpoint.Address, endpoint.Parameters)
	return err
}

func endpointConnection(address string, parameters map[string]string) (string, string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX endpoint", "endpoint address is required")
	}
	if host, port, err := net.SplitHostPort(address); err == nil {
		if host == "" || port == "" {
			return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX endpoint", "endpoint address must contain a host and port")
		}
		if _, err := strconv.Atoi(port); err != nil {
			return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX endpoint", "endpoint port must be numeric")
		}
		return host, port, nil
	}
	port := strconv.Itoa(defaultPort)
	if parameters != nil && strings.TrimSpace(parameters[parameterPort]) != "" {
		port = strings.TrimSpace(parameters[parameterPort])
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX endpoint", "endpoint port must be numeric")
	}
	if strings.Contains(address, ":") {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX endpoint", "endpoint address must use host:port for IPv6 or a port parameter for a hostname")
	}
	return address, port, nil
}

func runtimePaths(runtime *acceleratorv1alpha1.ClientRuntimeSpec) (string, string, error) {
	if runtime == nil {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX provider", "clientRuntime is required")
	}
	sourcePath := runtime.SourcePath
	if sourcePath == "" {
		sourcePath = defaultSourcePath
	}
	mountPath := runtime.MountPath
	if mountPath == "" {
		mountPath = defaultMountPath
	}
	if !path.IsAbs(sourcePath) || !path.IsAbs(mountPath) || path.Clean(sourcePath) == "/" || path.Clean(mountPath) == "/" {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate CoreX provider", "runtime sourcePath and mountPath must be non-root absolute paths")
	}
	return path.Clean(sourcePath), path.Clean(mountPath), nil
}

func remoteAllocationMode(config *acceleratorv1alpha1.RemotePoolConfig) acceleratorv1alpha1.RemoteAllocationMode {
	if config == nil || config.AllocationMode == "" {
		return acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint
	}
	return config.AllocationMode
}
