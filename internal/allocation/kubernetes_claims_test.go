package allocation

import (
	"context"
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesEndpointClaimerIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	scheme := claimTestScheme(t)
	ownerA := claimOwnerLease("lease-a", "holder-a", acceleratorv1alpha1.AllocationPhaseBound)
	ownerB := claimOwnerLease("lease-b", "holder-b", acceleratorv1alpha1.AllocationPhasePending)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ownerA, ownerB).Build()
	claimer := &KubernetesEndpointClaimer{
		Client: kubeClient, APIReader: kubeClient, Namespace: "accelerator-system",
		Now: func() time.Time { return time.Unix(100, 0) },
	}
	claimA := EndpointClaim{
		EndpointName: "gpu-a", HolderKey: "holder-a", LeaseNamespace: "default", LeaseName: "lease-a", TTL: time.Minute,
	}
	if err := claimer.Claim(ctx, claimA); err != nil {
		t.Fatalf("first Claim returned error: %v", err)
	}
	if err := claimer.Claim(ctx, claimA); err != nil {
		t.Fatalf("idempotent Claim returned error: %v", err)
	}
	claimB := EndpointClaim{
		EndpointName: "gpu-a", HolderKey: "holder-b", LeaseNamespace: "default", LeaseName: "lease-b", TTL: time.Minute,
	}
	if err := claimer.Claim(ctx, claimB); !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
		t.Fatalf("expected competing holder conflict, got %v", err)
	}
	if err := claimer.Release(ctx, claimB); !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
		t.Fatalf("expected non-owner release conflict, got %v", err)
	}
	if err := claimer.Release(ctx, claimA); err != nil {
		t.Fatalf("owner Release returned error: %v", err)
	}
	stored := &coordinationv1.Lease{}
	err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "accelerator-system", Name: endpointClaimName("gpu-a")}, stored)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected endpoint claim to be deleted, got %v", err)
	}
}

func TestKubernetesEndpointClaimerReclaimsOrphanedClaim(t *testing.T) {
	ctx := context.Background()
	scheme := claimTestScheme(t)
	oldHolder := "missing-holder"
	orphan := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: endpointClaimName("gpu-a"), Namespace: "accelerator-system",
			Annotations: map[string]string{
				claimEndpointAnnotation:       "gpu-a",
				claimLeaseNamespaceAnnotation: "default",
				claimLeaseNameAnnotation:      "missing-lease",
			},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &oldHolder},
	}
	owner := claimOwnerLease("lease-new", "holder-new", acceleratorv1alpha1.AllocationPhasePending)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(orphan, owner).Build()
	claimer := &KubernetesEndpointClaimer{Client: kubeClient, APIReader: kubeClient, Namespace: "accelerator-system"}
	claim := EndpointClaim{
		EndpointName: "gpu-a", HolderKey: "holder-new", LeaseNamespace: "default", LeaseName: "lease-new", TTL: time.Minute,
	}
	if err := claimer.Claim(ctx, claim); err != nil {
		t.Fatalf("reclaim orphan returned error: %v", err)
	}
	stored := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "accelerator-system", Name: endpointClaimName("gpu-a")}, stored); err != nil {
		t.Fatalf("read replacement claim: %v", err)
	}
	if stored.Spec.HolderIdentity == nil || *stored.Spec.HolderIdentity != "holder-new" {
		t.Fatalf("unexpected replacement holder: %#v", stored.Spec.HolderIdentity)
	}
}

func claimTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coordination scheme: %v", err)
	}
	if err := acceleratorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add accelerator scheme: %v", err)
	}
	return scheme
}

func claimOwnerLease(name, uid string, phase acceleratorv1alpha1.AllocationPhase) *acceleratorv1alpha1.AcceleratorLease {
	return &acceleratorv1alpha1.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Status:     acceleratorv1alpha1.AcceleratorLeaseStatus{Phase: phase},
	}
}
