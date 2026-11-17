package cricket

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

const (
	DriverName        = "cricket"
	ParameterRPCID    = "rpcID"
	defaultSourcePath = "/opt/cricket/bin"
	defaultMountPath  = "/opt/accelerator/cricket"
	clientLibraryName = "cricket-client.so"
	runtimeVolumeName = "accelerator-runtime"
	runtimeInitName   = "accelerator-runtime-setup"
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
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", "provider and pool are required")
	}
	if err := p.ValidateProvider(provider); err != nil {
		return err
	}
	if pool.Spec.Location != acceleratorv1alpha1.ExecutionModeRemote || pool.Spec.Remote == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", "a Remote pool is required")
	}
	if pool.Spec.DeviceType != acceleratorv1alpha1.DeviceKindGPU || !supportsKind(provider.Spec.DeviceKinds, acceleratorv1alpha1.DeviceKindGPU) {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate Cricket provider", "Cricket only supports GPU pools")
	}
	if remoteAllocationMode(pool.Spec.Remote) != acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate Cricket provider", "Cricket requires ExclusiveEndpoint allocation")
	}
	return nil
}

func (p *Provider) ValidateProvider(provider *acceleratorv1alpha1.AcceleratorProvider) error {
	if provider == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", "provider is required")
	}
	if provider.Spec.Driver != DriverName {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", fmt.Sprintf("driver must be %q", DriverName))
	}
	if provider.Spec.ClientRuntime == nil || strings.TrimSpace(provider.Spec.ClientRuntime.Image) == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", "clientRuntime.image is required")
	}
	if strings.TrimSpace(provider.Spec.ClientRuntime.RuntimeVersion) == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", "clientRuntime.runtimeVersion is required")
	}
	if _, _, err := clientRuntimePaths(provider.Spec.ClientRuntime); err != nil {
		return err
	}
	if provider.Spec.TrustBundleSecretRef != nil {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate Cricket provider", "Cricket does not provide a TLS transport")
	}
	if provider.Spec.AuthSecretRef != nil {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate Cricket provider", "Cricket does not provide an authenticated transport")
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
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build Cricket runtime", "a Remote accelerator lease is required")
	}
	if len(lease.Spec.Targets) != 1 || !lease.Spec.Targets[0].Exclusive {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindUnsupported, "build Cricket runtime", "Cricket requires exactly one exclusive endpoint target")
	}
	if lease.Status.Binding.ID == "" || len(lease.Status.Binding.Endpoints) != 1 {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build Cricket runtime", "the lease requires one resolved endpoint binding")
	}
	binding := lease.Status.Binding.Endpoints[0]
	if binding.Name == "" || binding.Name != lease.Spec.Targets[0].EndpointName {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build Cricket runtime", "the endpoint binding does not match the allocation target")
	}
	if binding.Runtime.Version == "" || binding.Runtime.Version != provider.Spec.ClientRuntime.RuntimeVersion {
		return contracts.RuntimePlan{}, contracts.NewError(
			contracts.ErrorKindUnsupported,
			"build Cricket runtime",
			fmt.Sprintf("endpoint runtime version %q is incompatible with client runtime version %q", binding.Runtime.Version, provider.Spec.ClientRuntime.RuntimeVersion),
		)
	}
	address, rpcID, err := endpointConnection(contracts.RemoteEndpoint{
		Name:       binding.Name,
		Address:    binding.Address,
		Runtime:    binding.Runtime,
		Parameters: binding.Parameters,
	})
	if err != nil {
		return contracts.RuntimePlan{}, err
	}

	sourcePath, mountPath, err := clientRuntimePaths(provider.Spec.ClientRuntime)
	if err != nil {
		return contracts.RuntimePlan{}, err
	}

	environment := []corev1.EnvVar{
		{Name: "REMOTE_GPU_ADDRESS", Value: address},
		{Name: "LD_PRELOAD", Value: path.Join(mountPath, clientLibraryName)},
		{Name: "LD_LIBRARY_PATH", Value: mountPath},
	}
	if rpcID != "" {
		environment = append(environment, corev1.EnvVar{Name: "CRICKET_RPCID", Value: rpcID})
	}

	return contracts.RuntimePlan{
		Mode:          acceleratorv1alpha1.ExecutionModeRemote,
		ContainerName: lease.Spec.Request.ContainerName,
		Volumes: []corev1.Volume{{
			Name: runtimeVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
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
		Env: environment,
		VolumeMounts: []corev1.VolumeMount{{
			Name:      runtimeVolumeName,
			MountPath: mountPath,
			ReadOnly:  true,
		}},
		Annotations: map[string]string{
			"accelerator.qcrs.io/mode":     string(acceleratorv1alpha1.ExecutionModeRemote),
			"accelerator.qcrs.io/provider": lease.Spec.ProviderRef.Name,
			"accelerator.qcrs.io/pool":     lease.Spec.PoolRef.Name,
			"accelerator.qcrs.io/lease":    lease.Name,
			"accelerator.qcrs.io/endpoint": binding.Name,
		},
	}, nil
}

func (p *Provider) ValidateEndpoint(endpoint contracts.RemoteEndpoint) error {
	_, _, err := endpointConnection(endpoint)
	return err
}

func endpointConnection(endpoint contracts.RemoteEndpoint) (string, string, error) {
	if endpoint.Runtime.Driver != DriverName {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket endpoint", fmt.Sprintf("runtime driver must be %q", DriverName))
	}
	if strings.TrimSpace(endpoint.Runtime.Version) == "" {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket endpoint", "runtime.version is required")
	}
	address, err := normalizeAddress(endpoint.Address)
	if err != nil {
		return "", "", err
	}
	rpcID, err := cricketRPCID(endpoint.Parameters)
	if err != nil {
		return "", "", err
	}
	effectiveRPCID := rpcID
	if effectiveRPCID == "" {
		effectiveRPCID = "1"
	}
	if apiVersion := strings.TrimSpace(endpoint.Runtime.APIVersion); apiVersion != "" {
		value, parseErr := strconv.ParseUint(apiVersion, 10, 32)
		if parseErr != nil || value == 0 || strconv.FormatUint(value, 10) != effectiveRPCID {
			return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket endpoint", "runtime.apiVersion must match the effective Cricket rpcID")
		}
	}
	return address, rpcID, nil
}

func clientRuntimePaths(runtime *acceleratorv1alpha1.ClientRuntimeSpec) (string, string, error) {
	if runtime == nil {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", "clientRuntime is required")
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
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", "runtime sourcePath and mountPath must be non-root absolute paths")
	}
	sourcePath = path.Clean(sourcePath)
	mountPath = path.Clean(mountPath)
	if sourcePath == runtimeCopyPath || strings.HasPrefix(sourcePath, runtimeCopyPath+"/") {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket provider", fmt.Sprintf("runtime sourcePath must not be inside the init-container mount %q", runtimeCopyPath))
	}
	return sourcePath, mountPath, nil
}

func normalizeAddress(raw string) (string, error) {
	address := strings.TrimSpace(raw)
	if address == "" || len(address) > 255 || strings.ContainsAny(address, "/?#@, \t\r\n") || strings.Contains(address, "://") {
		return "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket endpoint", "address must be a host or host:port no longer than 255 bytes")
	}
	if strings.Count(address, ":") > 1 {
		return "", contracts.NewError(contracts.ErrorKindUnsupported, "validate Cricket endpoint", "the current Cricket client does not parse IPv6 addresses")
	}
	if host, port, found := strings.Cut(address, ":"); found {
		if host == "" || port == "" {
			return "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket endpoint", "host and port must both be present")
		}
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket endpoint", "port must be between 1 and 65535")
		}
	}
	return address, nil
}

func cricketRPCID(parameters map[string]string) (string, error) {
	for key := range parameters {
		if key != ParameterRPCID {
			return "", contracts.NewError(contracts.ErrorKindUnsupported, "validate Cricket endpoint", fmt.Sprintf("unknown endpoint parameter %q", key))
		}
	}
	raw := strings.TrimSpace(parameters[ParameterRPCID])
	if raw == "" {
		return "", nil
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || value == 0 {
		return "", contracts.NewError(contracts.ErrorKindInvalid, "validate Cricket endpoint", "rpcID must be an unsigned non-zero 32-bit integer")
	}
	return strconv.FormatUint(value, 10), nil
}

func remoteAllocationMode(config *acceleratorv1alpha1.RemotePoolConfig) acceleratorv1alpha1.RemoteAllocationMode {
	if config == nil || config.AllocationMode == "" {
		return acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint
	}
	return config.AllocationMode
}

func supportsKind(kinds []acceleratorv1alpha1.DeviceKind, wanted acceleratorv1alpha1.DeviceKind) bool {
	for _, kind := range kinds {
		if kind == wanted {
			return true
		}
	}
	return false
}
