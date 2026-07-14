package lupine

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

const (
	DriverName           = "lupine"
	defaultSourcePath    = "/opt/lupine/lib"
	defaultMountPath     = "/opt/accelerator/lupine"
	runtimeVolumeName    = "accelerator-runtime"
	runtimeInitName      = "accelerator-runtime-setup"
	trustVolumeName      = "accelerator-trust-bundle"
	trustMountPath       = "/var/run/accelerator/trust"
	maximumServerEntries = 16
)

type SecretResolver interface {
	Resolve(ctx context.Context, source corev1.SecretReference, targetNamespace string) (string, error)
}

type SameNamespaceSecretResolver struct{}

func (SameNamespaceSecretResolver) Resolve(_ context.Context, source corev1.SecretReference, targetNamespace string) (string, error) {
	if source.Namespace != "" && source.Namespace != targetNamespace {
		return "", contracts.NewError(contracts.ErrorKindUnsupported, "resolve trust bundle", "cross-namespace Secret materialization is not implemented")
	}
	if source.Name == "" {
		return "", contracts.NewError(contracts.ErrorKindInvalid, "resolve trust bundle", "Secret name is empty")
	}
	return source.Name, nil
}

type Provider struct {
	Secrets SecretResolver
}

func New(secretResolver SecretResolver) *Provider {
	if secretResolver == nil {
		secretResolver = SameNamespaceSecretResolver{}
	}
	return &Provider{Secrets: secretResolver}
}

func (p *Provider) Driver() string { return DriverName }

func (p *Provider) Capabilities() contracts.CapabilitySet {
	return contracts.CapabilitySet{
		DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
		Modes:       []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeRemote},
		SupportsTLS: true,
	}
}

func (p *Provider) Validate(provider *acceleratorv1alpha1.AcceleratorProvider, pool *acceleratorv1alpha1.AcceleratorPool) error {
	if provider == nil || pool == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Lupine provider", "provider and pool are required")
	}
	if provider.Spec.Driver != DriverName {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Lupine provider", fmt.Sprintf("driver must be %q", DriverName))
	}
	if pool.Spec.Location != acceleratorv1alpha1.ExecutionModeRemote || pool.Spec.Remote == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Lupine provider", "a Remote pool is required")
	}
	if pool.Spec.DeviceType != acceleratorv1alpha1.DeviceKindGPU {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate Lupine provider", "Lupine only supports GPU pools")
	}
	if !supportsKind(provider.Spec.DeviceKinds, acceleratorv1alpha1.DeviceKindGPU) {
		return contracts.NewError(contracts.ErrorKindUnsupported, "validate Lupine provider", "provider does not declare GPU support")
	}
	if provider.Spec.ClientRuntime == nil || provider.Spec.ClientRuntime.Image == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Lupine provider", "clientRuntime.image is required")
	}
	return nil
}

func supportsKind(kinds []acceleratorv1alpha1.DeviceKind, wanted acceleratorv1alpha1.DeviceKind) bool {
	for _, kind := range kinds {
		if kind == wanted {
			return true
		}
	}
	return false
}

func (p *Provider) BuildRuntimePlan(
	ctx context.Context,
	provider *acceleratorv1alpha1.AcceleratorProvider,
	pool *acceleratorv1alpha1.AcceleratorPool,
	lease *acceleratorv1alpha1.AcceleratorLease,
) (contracts.RuntimePlan, error) {
	if err := p.Validate(provider, pool); err != nil {
		return contracts.RuntimePlan{}, err
	}
	if lease == nil || lease.Spec.Mode != acceleratorv1alpha1.ExecutionModeRemote {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build Lupine runtime", "a Remote accelerator lease is required")
	}
	servers, err := ServerList(lease.Status.Binding.Endpoints)
	if err != nil {
		return contracts.RuntimePlan{}, err
	}

	sourcePath := provider.Spec.ClientRuntime.SourcePath
	if sourcePath == "" {
		sourcePath = defaultSourcePath
	}
	mountPath := provider.Spec.ClientRuntime.MountPath
	if mountPath == "" {
		mountPath = defaultMountPath
	}
	if !path.IsAbs(sourcePath) || !path.IsAbs(mountPath) {
		return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindInvalid, "build Lupine runtime", "runtime sourcePath and mountPath must be absolute")
	}

	plan := contracts.RuntimePlan{
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
			Args:    []string{"-a", strings.TrimSuffix(sourcePath, "/") + "/.", "/runtime/"},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      runtimeVolumeName,
				MountPath: "/runtime",
			}},
		}},
		Env: []corev1.EnvVar{
			{Name: "LUPINE_SERVER", Value: servers},
			{Name: "LD_LIBRARY_PATH", Value: mountPath},
		},
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
		},
	}

	if provider.Spec.TrustBundleSecretRef != nil {
		secretName, err := p.Secrets.Resolve(ctx, *provider.Spec.TrustBundleSecretRef, lease.Namespace)
		if err != nil {
			return contracts.RuntimePlan{}, err
		}
		plan.Volumes = append(plan.Volumes, corev1.Volume{
			Name: trustVolumeName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
				Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
			}},
		})
		plan.VolumeMounts = append(plan.VolumeMounts, corev1.VolumeMount{
			Name:      trustVolumeName,
			MountPath: trustMountPath,
			ReadOnly:  true,
		})
		plan.Env = append(plan.Env, corev1.EnvVar{
			Name:  "SSL_CERT_FILE",
			Value: trustMountPath + "/ca.crt",
		})
	}
	return plan, nil
}

func ServerList(endpoints []string) (string, error) {
	if len(endpoints) == 0 {
		return "", contracts.NewError(contracts.ErrorKindInvalid, "build Lupine server list", "at least one endpoint is required")
	}
	if len(endpoints) > maximumServerEntries {
		return "", contracts.NewError(contracts.ErrorKindUnsupported, "build Lupine server list", fmt.Sprintf("Lupine supports at most %d server entries", maximumServerEntries))
	}
	normalized := make([]string, len(endpoints))
	for index, endpoint := range endpoints {
		value := strings.TrimSpace(endpoint)
		if err := validateEndpoint(value); err != nil {
			return "", fmt.Errorf("endpoint %d: %w", index, err)
		}
		normalized[index] = value
	}
	return strings.Join(normalized, ","), nil
}

func validateEndpoint(endpoint string) error {
	if endpoint == "" || strings.Contains(endpoint, ",") {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Lupine endpoint", "endpoint is empty or contains a comma")
	}
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return contracts.WrapError(contracts.ErrorKindInvalid, "validate Lupine endpoint", "invalid URI", err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return contracts.NewError(contracts.ErrorKindUnsupported, "validate Lupine endpoint", fmt.Sprintf("scheme %q is unsupported", parsed.Scheme))
		}
		if parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return contracts.NewError(contracts.ErrorKindInvalid, "validate Lupine endpoint", "URI must contain only scheme, host, and optional port without credentials")
		}
		return validatePort(parsed.Port())
	}

	parsed, err := url.Parse("//" + endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "validate Lupine endpoint", "plain endpoint must be host or host:port")
	}
	return validatePort(parsed.Port())
}

func validatePort(port string) error {
	if port == "" {
		return nil
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return contracts.WrapError(contracts.ErrorKindInvalid, "validate Lupine endpoint", "invalid port", err)
	}
	return nil
}
