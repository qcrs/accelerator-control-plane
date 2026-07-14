package injector

import (
	"reflect"
	"testing"

	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
)

func TestMutateIsIdempotent(t *testing.T) {
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "worker", Image: "example"}},
	}}
	plan := contracts.RuntimePlan{
		ContainerName: "worker",
		Volumes: []corev1.Volume{{
			Name:         "accelerator-runtime",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
		InitContainers: []corev1.Container{{Name: "accelerator-runtime-setup", Image: "runtime"}},
		Env: []corev1.EnvVar{
			{Name: "LUPINE_SERVER", Value: "https://gpu.example"},
			{Name: "LD_LIBRARY_PATH", Value: "/opt/accelerator/lupine"},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "accelerator-runtime", MountPath: "/opt/accelerator/lupine", ReadOnly: true}},
		Annotations:  map[string]string{"accelerator.qcrs.io/mode": "Remote"},
	}
	mutator := NewPodMutator()
	first, err := mutator.Mutate(template, plan)
	if err != nil {
		t.Fatalf("first mutation failed: %v", err)
	}
	second, err := mutator.Mutate(first, plan)
	if err != nil {
		t.Fatalf("second mutation failed: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("mutation is not idempotent")
	}
	patch, err := mutator.JSONPatch(template, first)
	if err != nil || len(patch) == 0 || string(patch) == "[]" {
		t.Fatalf("expected non-empty JSON patch, got %q, %v", patch, err)
	}
}

func TestMutateRejectsManagedEnvironmentConflict(t *testing.T) {
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "worker",
			Env:  []corev1.EnvVar{{Name: "LUPINE_SERVER", Value: "other:14833"}},
		}},
	}}
	_, err := NewPodMutator().Mutate(template, contracts.RuntimePlan{
		ContainerName: "worker",
		Env:           []corev1.EnvVar{{Name: "LUPINE_SERVER", Value: "selected:14833"}},
	})
	if !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("expected invalid conflict, got %v", err)
	}
}

func TestMutateAddsLocalResourceAndNodeAffinity(t *testing.T) {
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "worker"}},
	}}
	resources := ResourceRequirements("nvidia.com/gpu", 2)
	mutated, err := NewPodMutator().Mutate(template, contracts.RuntimePlan{
		ContainerName:    "worker",
		Resources:        &resources,
		RequiredNodeName: "node-a",
	})
	if err != nil {
		t.Fatalf("mutation failed: %v", err)
	}
	if got := mutated.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; got.Value() != 2 {
		t.Fatalf("expected two GPUs, got %s", got.String())
	}
	terms := mutated.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 || terms[0].MatchExpressions[0].Values[0] != "node-a" {
		t.Fatalf("unexpected node affinity: %#v", terms)
	}
}

func TestMutateNarrowsExistingNodeAffinityToSelectedNode(t *testing.T) {
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "worker"}},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: hostnameLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a", "node-b"},
					}},
				}},
			},
		}},
	}}
	mutated, err := NewPodMutator().Mutate(template, contracts.RuntimePlan{
		ContainerName:    "worker",
		RequiredNodeName: "node-a",
	})
	if err != nil {
		t.Fatalf("mutation failed: %v", err)
	}
	expressions := mutated.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
	if len(expressions) != 2 || len(expressions[1].Values) != 1 || expressions[1].Values[0] != "node-a" {
		t.Fatalf("selected node was not enforced exactly: %#v", expressions)
	}
}
