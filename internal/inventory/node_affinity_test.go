package inventory

import (
	"testing"

	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"github.com/qcrs/accelerator-control-plane/internal/injector"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEmptyNodeSelectorTermsMatchNoNodes(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"zone": "east"}}}
	for _, terms := range [][]corev1.NodeSelectorTerm{
		nil, {{}}, {{}, {MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"west"}}}}},
	} {
		spec := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms},
		}}}
		if nodeMatchesPod(node, spec) {
			t.Errorf("empty affinity term broadened scheduling eligibility: %+v", terms)
		}
	}
}

func TestSelectedNodeRemainsSchedulableAfterInjection(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a.example", Labels: map[string]string{
		corev1.LabelHostname: "worker-a", "zone": "east",
	}}}
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers:   []corev1.Container{{Name: "worker"}},
		NodeSelector: map[string]string{corev1.LabelHostname: "worker-a"},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{}, {MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"east"}}}},
			}},
		}},
	}}
	if !nodeMatchesPod(node, &template.Spec) {
		t.Fatal("test node is not an eligible selection")
	}
	mutated, err := injector.NewPodMutator().Mutate(template, contracts.RuntimePlan{ContainerName: "worker", RequiredNodeName: node.Name})
	if err != nil {
		t.Fatalf("selected Node name was incorrectly compared with its hostname label: %v", err)
	}
	if !nodeMatchesPod(node, &mutated.Spec) {
		t.Fatal("runtime injection made the selected node unschedulable")
	}
	for _, other := range []*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "different-node", Labels: node.Labels}},
		{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Labels: map[string]string{corev1.LabelHostname: "worker-a", "zone": "west"}}},
	} {
		if nodeMatchesPod(other, &mutated.Spec) {
			t.Fatalf("runtime injection broadened the original affinity or changed the selected node: %+v", other.ObjectMeta)
		}
	}
	if mutated.Spec.NodeName != "" {
		t.Fatal("runtime injection bypassed the Kubernetes scheduler")
	}
}
