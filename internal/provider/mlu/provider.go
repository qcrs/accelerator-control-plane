// Package mlu provides the ARM64 MLU remote-runtime adapter.
package mlu

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
	DriverName        = "mlu-remote"
	defaultSourcePath = "/opt/mlu-remote/lib"
	defaultMountPath  = "/opt/accelerator/mlu"
	defaultRemotePort = 50051
	parameterPort     = "port"
	runtimeInitName   = "accelerator-runtime-setup"
	runtimeVolumeName = "accelerator-runtime"
	runtimeCopyPath   = "/runtime"
	cnrtShimRelative  = "shim/libcnrt.so"
	cndrvShimRelative = "driver-shim/libcndrv.so"
)

type Provider struct{}

func New() *Provider { return &Provider{} }

func (p *Provider) Driver() string { return DriverName }

func (p *Provider) Capabilities() contracts.CapabilitySet {
	return contracts.CapabilitySet{
		DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindNPU},
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
		return contracts.NewError(contracts.ErrorKindInvalid, "validate MLU provider", "provider and pool are required")
	}
	if err := p.ValidateProvider(provider); err != nil {
		return err
	}
	if pool.Spec.Location != acceleratorv1alpha1.ExecutionModeRemote || pool.Spec.Remote == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate MLU provider", "a Remote pool is required")
	}
	if pool.Spec.DeviceType != acceleratorv1alpha1.DeviceKindNPU || !containsKind(provider.Spec.DeviceKinds, acceleratorv1alpha1.DeviceKindNPU) {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate MLU provider", "MLU remote requires an NPU pool")
	}
	if remoteAllocationMode(pool.Spec.Remote) != acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate MLU provider", "MLU remote currently requires ExclusiveEndpoint allocation")
	}
	return nil
}

func (p *Provider) ValidateProvider(provider *acceleratorv1alpha1.AcceleratorProvider) error {
	if provider == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate MLU provider", "provider is required")
	}
	if provider.Spec.Driver != DriverName {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate MLU provider", fmt.Sprintf("driver must be %q", DriverName))
	}
	if provider.Spec.ClientRuntime == nil || strings.TrimSpace(provider.Spec.ClientRuntime.Image) == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate MLU provider", "clientRuntime.image is required")
	}
	if strings.TrimSpace(provider.Spec.ClientRuntime.RuntimeVersion) == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate MLU provider", "clientRuntime.runtimeVersion is required")
	}
	if _, _, err := runtimePaths(provider.Spec.ClientRuntime); err != nil {
		return err
	}
	if provider.Spec.TrustBundleSecretRef != nil {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate MLU provider", "the current MLU transport does not provide TLS")
	}
	if provider.Spec.AuthSecretRef != nil {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate MLU provider", "the current MLU transport does not provide authentication")
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
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build MLU runtime", "a Remote accelerator lease is required")
	}
	if len(lease.Spec.Targets) != 1 || !lease.Spec.Targets[0].Exclusive {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindUnsupported, "build MLU runtime", "MLU remote requires exactly one exclusive endpoint target")
	}
	if lease.Status.Binding.ID == "" || len(lease.Status.Binding.Endpoints) != 1 {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build MLU runtime", "the lease requires one resolved endpoint binding")
	}
	binding := lease.Status.Binding.Endpoints[0]
	target := lease.Spec.Targets[0]
	if binding.Name == "" || binding.Name != target.EndpointName {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build MLU runtime", "the endpoint binding does not match the allocation target")
	}
	if binding.Runtime.Version == "" || binding.Runtime.Version != provider.Spec.ClientRuntime.RuntimeVersion {
		return contracts.RuntimePlan{}, contracts.NewError(
			contracts.ErrorKindUnsupported,
			"build MLU runtime",
			fmt.Sprintf("endpoint runtime version %q is incompatible with client runtime version %q", binding.Runtime.Version, provider.Spec.ClientRuntime.RuntimeVersion),
		)
	}
	host, port, err := endpointConnection(binding.Address, binding.Parameters)
	if err != nil {
		return contracts.RuntimePlan{}, err
	}
	sourcePath, mountPath, err := runtimePaths(provider.Spec.ClientRuntime)
	if err != nil {
		return contracts.RuntimePlan{}, err
	}

	return contracts.RuntimePlan{
		Mode:          acceleratorv1alpha1.ExecutionModeRemote,
		ContainerName: lease.Spec.Request.ContainerName,
		Volumes: []corev1.Volume{{
			Name:         runtimeVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
		InitContainers: []corev1.Container{{
			Name:    runtimeInitName,
			Image:   provider.Spec.ClientRuntime.Image,
			Command: []string{"/bin/cp"},
			Args:    []string{"-a", strings.TrimSuffix(sourcePath, "/") + "/.", runtimeCopyPath + "/"},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      runtimeVolumeName,
				MountPath: runtimeCopyPath,
			}},
		}},
		Env: []corev1.EnvVar{
			{Name: "MLU_REMOTE_HOST", Value: host},
			{Name: "MLU_REMOTE_PORT", Value: port},
			{Name: "LD_PRELOAD", Value: path.Join(mountPath, cnrtShimRelative) + ":" + path.Join(mountPath, cndrvShimRelative)},
			{Name: "LD_LIBRARY_PATH", Value: strings.Join([]string{
				mountPath,
				path.Join(mountPath, "shim"),
				path.Join(mountPath, "driver-shim"),
			}, ":")},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      runtimeVolumeName,
			MountPath: mountPath,
			ReadOnly:  true,
		}},
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
	_, _, err := endpointConnection(endpoint.Address, endpoint.Parameters)
	if err != nil {
		return err
	}
	if endpoint.Runtime.Driver != DriverName {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate MLU endpoint", fmt.Sprintf("runtime driver must be %q", DriverName))
	}
	if strings.TrimSpace(endpoint.Runtime.Version) == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate MLU endpoint", "runtime.version is required")
	}
	return nil
}

func endpointConnection(address string, parameters map[string]string) (string, string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate MLU endpoint", "endpoint address is required")
	}
	if host, port, err := net.SplitHostPort(address); err == nil {
		if host == "" || port == "" {
			return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate MLU endpoint", "endpoint address must contain a host and port")
		}
		if _, err := strconv.Atoi(port); err != nil {
			return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate MLU endpoint", "endpoint port must be numeric")
		}
		return host, port, nil
	}
	port := strconv.Itoa(defaultRemotePort)
	if parameters != nil && strings.TrimSpace(parameters[parameterPort]) != "" {
		port = strings.TrimSpace(parameters[parameterPort])
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate MLU endpoint", "endpoint port must be numeric")
	}
	if strings.Contains(address, ":") {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate MLU endpoint", "endpoint address must use host:port for IPv6 or a port parameter for a hostname")
	}
	return address, port, nil
}

func runtimePaths(runtime *acceleratorv1alpha1.ClientRuntimeSpec) (string, string, error) {
	if runtime == nil {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate MLU provider", "clientRuntime is required")
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
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate MLU provider", "runtime sourcePath and mountPath must be non-root absolute paths")
	}
	return path.Clean(sourcePath), path.Clean(mountPath), nil
}

func containsKind(kinds []acceleratorv1alpha1.DeviceKind, wanted acceleratorv1alpha1.DeviceKind) bool {
	for _, kind := range kinds {
		if kind == wanted {
			return true
		}
	}
	return false
}

func remoteAllocationMode(config *acceleratorv1alpha1.RemotePoolConfig) acceleratorv1alpha1.RemoteAllocationMode {
	if config == nil || config.AllocationMode == "" {
		return acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint
	}
	return config.AllocationMode
}
