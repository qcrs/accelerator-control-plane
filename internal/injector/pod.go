package injector

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const hostnameLabel = "kubernetes.io/hostname"

type PodMutator struct{}

func NewPodMutator() *PodMutator { return &PodMutator{} }

func (m *PodMutator) Mutate(template corev1.PodTemplateSpec, plan contracts.RuntimePlan) (corev1.PodTemplateSpec, error) {
	mutated := *template.DeepCopy()
	container, err := targetContainer(&mutated, plan.ContainerName)
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}

	if mutated.Annotations == nil {
		mutated.Annotations = map[string]string{}
	}
	for key, value := range plan.Annotations {
		if existing, found := mutated.Annotations[key]; found && existing != value {
			return corev1.PodTemplateSpec{}, conflict("annotation", key)
		}
		mutated.Annotations[key] = value
	}

	for _, volume := range plan.Volumes {
		if err := mergeVolume(&mutated.Spec.Volumes, volume); err != nil {
			return corev1.PodTemplateSpec{}, err
		}
	}
	for _, initContainer := range plan.InitContainers {
		if err := mergeInitContainer(&mutated.Spec.InitContainers, initContainer); err != nil {
			return corev1.PodTemplateSpec{}, err
		}
	}
	for _, env := range plan.Env {
		if err := mergeEnv(&container.Env, env); err != nil {
			return corev1.PodTemplateSpec{}, err
		}
	}
	for _, mount := range plan.VolumeMounts {
		if err := mergeVolumeMount(&container.VolumeMounts, mount); err != nil {
			return corev1.PodTemplateSpec{}, err
		}
	}
	if plan.Resources != nil {
		if err := mergeResources(&container.Resources, *plan.Resources); err != nil {
			return corev1.PodTemplateSpec{}, err
		}
	}
	if plan.RequiredNodeName != "" {
		if err := requireNode(&mutated.Spec, plan.RequiredNodeName); err != nil {
			return corev1.PodTemplateSpec{}, err
		}
	}
	return mutated, nil
}

func (m *PodMutator) JSONPatch(before, after corev1.PodTemplateSpec) ([]byte, error) {
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		return nil, fmt.Errorf("marshal original pod template: %w", err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return nil, fmt.Errorf("marshal mutated pod template: %w", err)
	}
	operations, err := jsonpatch.CreatePatch(beforeJSON, afterJSON)
	if err != nil {
		return nil, fmt.Errorf("create pod template JSON patch: %w", err)
	}
	patch, err := json.Marshal(operations)
	if err != nil {
		return nil, fmt.Errorf("marshal pod template JSON patch: %w", err)
	}
	return patch, nil
}

func targetContainer(template *corev1.PodTemplateSpec, name string) (*corev1.Container, error) {
	if len(template.Spec.Containers) == 0 {
		return nil, contracts.NewError(contracts.ErrorKindInvalid, "mutate pod", "pod template has no regular containers")
	}
	if name == "" {
		return &template.Spec.Containers[0], nil
	}
	for index := range template.Spec.Containers {
		if template.Spec.Containers[index].Name == name {
			return &template.Spec.Containers[index], nil
		}
	}
	return nil, contracts.NewError(contracts.ErrorKindInvalid, "mutate pod", fmt.Sprintf("target container %q does not exist", name))
}

func mergeVolume(volumes *[]corev1.Volume, desired corev1.Volume) error {
	for index := range *volumes {
		if (*volumes)[index].Name != desired.Name {
			continue
		}
		if reflect.DeepEqual((*volumes)[index], desired) {
			return nil
		}
		return conflict("volume", desired.Name)
	}
	*volumes = append(*volumes, desired)
	return nil
}

func mergeInitContainer(containers *[]corev1.Container, desired corev1.Container) error {
	for index := range *containers {
		if (*containers)[index].Name != desired.Name {
			continue
		}
		if reflect.DeepEqual((*containers)[index], desired) {
			return nil
		}
		return conflict("init container", desired.Name)
	}
	*containers = append(*containers, desired)
	return nil
}

func mergeEnv(values *[]corev1.EnvVar, desired corev1.EnvVar) error {
	for index := range *values {
		existing := &(*values)[index]
		if existing.Name != desired.Name {
			continue
		}
		if reflect.DeepEqual(*existing, desired) {
			return nil
		}
		if (desired.Name == "LD_LIBRARY_PATH" || desired.Name == "LD_PRELOAD") && existing.ValueFrom == nil && desired.ValueFrom == nil {
			return prependPathLikeValue(existing, desired.Value)
		}
		return conflict("environment variable", desired.Name)
	}
	*values = append(*values, desired)
	return nil
}

func prependPathLikeValue(existing *corev1.EnvVar, prefix string) error {
	prefix = strings.Trim(prefix, ": ")
	if prefix == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "mutate pod", fmt.Sprintf("managed %s value is empty", existing.Name))
	}
	for _, value := range strings.FieldsFunc(existing.Value, func(character rune) bool {
		return character == ':' || character == ' ' || character == '\t'
	}) {
		if value == prefix {
			return nil
		}
	}
	if existing.Value == "" {
		existing.Value = prefix
	} else {
		existing.Value = prefix + ":" + existing.Value
	}
	return nil
}

func mergeVolumeMount(mounts *[]corev1.VolumeMount, desired corev1.VolumeMount) error {
	for index := range *mounts {
		existing := (*mounts)[index]
		if existing.Name != desired.Name && existing.MountPath != desired.MountPath {
			continue
		}
		if reflect.DeepEqual(existing, desired) {
			return nil
		}
		return conflict("volume mount", desired.Name)
	}
	*mounts = append(*mounts, desired)
	return nil
}

func mergeResources(existing *corev1.ResourceRequirements, desired corev1.ResourceRequirements) error {
	if existing.Requests == nil {
		existing.Requests = corev1.ResourceList{}
	}
	if existing.Limits == nil {
		existing.Limits = corev1.ResourceList{}
	}
	if err := mergeResourceList(existing.Requests, desired.Requests, "request"); err != nil {
		return err
	}
	return mergeResourceList(existing.Limits, desired.Limits, "limit")
}

func mergeResourceList(existing, desired corev1.ResourceList, kind string) error {
	for name, quantity := range desired {
		if current, found := existing[name]; found && current.Cmp(quantity) != 0 {
			return conflict("resource "+kind, string(name))
		}
		existing[name] = quantity.DeepCopy()
	}
	return nil
}

func requireNode(spec *corev1.PodSpec, nodeName string) error {
	if spec.NodeName != "" && spec.NodeName != nodeName {
		return conflict("spec.nodeName", spec.NodeName)
	}
	if selected, found := spec.NodeSelector[hostnameLabel]; found && selected != nodeName {
		return conflict("node selector", hostnameLabel)
	}
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil {
		required = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = required
	}
	if len(required.NodeSelectorTerms) == 0 {
		required.NodeSelectorTerms = []corev1.NodeSelectorTerm{{}}
	}
	for index := range required.NodeSelectorTerms {
		term := &required.NodeSelectorTerms[index]
		if requirementExists(term.MatchExpressions, nodeName) {
			continue
		}
		term.MatchExpressions = append(term.MatchExpressions, corev1.NodeSelectorRequirement{
			Key:      hostnameLabel,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{nodeName},
		})
	}
	return nil
}

func requirementExists(requirements []corev1.NodeSelectorRequirement, nodeName string) bool {
	for _, requirement := range requirements {
		if requirement.Key == hostnameLabel &&
			requirement.Operator == corev1.NodeSelectorOpIn &&
			len(requirement.Values) == 1 && requirement.Values[0] == nodeName {
			return true
		}
	}
	return false
}

func conflict(kind, name string) error {
	return contracts.NewError(contracts.ErrorKindInvalid, "mutate pod", fmt.Sprintf("%s %q conflicts with accelerator-managed configuration", kind, name))
}

func ResourceRequirements(name corev1.ResourceName, count int32) corev1.ResourceRequirements {
	quantity := *resource.NewQuantity(int64(count), resource.DecimalSI)
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{name: quantity},
		Limits:   corev1.ResourceList{name: quantity},
	}
}
