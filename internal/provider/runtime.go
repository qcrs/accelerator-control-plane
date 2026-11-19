package provider

import (
	"path"
	"strings"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

const RuntimeInitContainerName = "accelerator-runtime-setup"

// ClientRuntimePaths validates the common library layout. Defaults and error
// context stay with the vendor Provider.
func ClientRuntimePaths(runtime *api.ClientRuntimeSpec, defaultSource, defaultMount, operation string) (string, string, error) {
	if runtime == nil {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, operation, "clientRuntime is required")
	}
	source, mount := runtime.SourcePath, runtime.MountPath
	if source == "" {
		source = defaultSource
	}
	if mount == "" {
		mount = defaultMount
	}
	if !path.IsAbs(source) || !path.IsAbs(mount) || path.Clean(source) == "/" || path.Clean(mount) == "/" {
		return "", "", contracts.NewError(contracts.ErrorKindInvalid, operation, "runtime sourcePath and mountPath must be non-root absolute paths")
	}
	return path.Clean(source), path.Clean(mount), nil
}

// LibraryRuntimePlan contains only the shared library copy and mounts. The
// Provider supplies mode, target container, environment, and annotations.
func LibraryRuntimePlan(image, source, mount string) contracts.RuntimePlan {
	const volume, copyPath = "accelerator-runtime", "/runtime"
	return contracts.RuntimePlan{
		Volumes: []corev1.Volume{{Name: volume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		InitContainers: []corev1.Container{{
			Name: RuntimeInitContainerName, Image: image, Command: []string{"/bin/cp"},
			Args:         []string{"-a", strings.TrimSuffix(source, "/") + "/.", copyPath + "/"},
			VolumeMounts: []corev1.VolumeMount{{Name: volume, MountPath: copyPath}},
		}},
		VolumeMounts: []corev1.VolumeMount{{Name: volume, MountPath: mount, ReadOnly: true}},
	}
}
