package allocation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultCoordinationNamespace  = "accelerator-system"
	endpointClaimPrefix           = "accelerator-endpoint-"
	claimEndpointAnnotation       = "accelerator.qcrs.io/endpoint"
	claimLeaseNamespaceAnnotation = "accelerator.qcrs.io/lease-namespace"
	claimLeaseNameAnnotation      = "accelerator.qcrs.io/lease-name"
)

// KubernetesEndpointClaimer uses a coordination.k8s.io Lease whose name is a
// stable hash of an Endpoint name. Create is the atomic compare-and-swap; the
// cached inventory view is never trusted as the final ownership decision.
type KubernetesEndpointClaimer struct {
	Client    client.Client
	APIReader client.Reader
	Namespace string
	Now       func() time.Time
}

func (c *KubernetesEndpointClaimer) Claim(ctx context.Context, claim EndpointClaim) error {
	if err := c.validate(claim); err != nil {
		return err
	}
	desired := c.claimObject(claim)
	if err := c.Client.Create(ctx, desired); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return contracts.WrapError(contracts.ErrorKindTransient, "claim remote endpoint", fmt.Sprintf("create endpoint claim %q", desired.Name), err)
	}

	existing := &coordinationv1.Lease{}
	key := client.ObjectKeyFromObject(desired)
	if err := c.reader().Get(ctx, key, existing); err != nil {
		return contracts.WrapError(contracts.ErrorKindTransient, "claim remote endpoint", fmt.Sprintf("read existing endpoint claim %q", desired.Name), err)
	}
	if existing.Spec.HolderIdentity != nil && *existing.Spec.HolderIdentity == claim.HolderKey {
		return nil
	}

	stale, err := c.claimIsStale(ctx, existing)
	if err != nil {
		return err
	}
	if !stale {
		holder := "unknown"
		if existing.Spec.HolderIdentity != nil && *existing.Spec.HolderIdentity != "" {
			holder = *existing.Spec.HolderIdentity
		}
		return contracts.NewError(contracts.ErrorKindConflict, "claim remote endpoint", fmt.Sprintf("endpoint %q is already held by allocation %s", claim.EndpointName, holder))
	}

	uid := existing.UID
	resourceVersion := existing.ResourceVersion
	if err := c.Client.Delete(ctx, existing, &client.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &resourceVersion,
	}}); err != nil && !apierrors.IsNotFound(err) {
		return contracts.WrapError(contracts.ErrorKindTransient, "claim remote endpoint", fmt.Sprintf("delete stale endpoint claim %q", existing.Name), err)
	}
	if err := c.Client.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return contracts.NewError(contracts.ErrorKindConflict, "claim remote endpoint", fmt.Sprintf("endpoint %q was claimed concurrently", claim.EndpointName))
		}
		return contracts.WrapError(contracts.ErrorKindTransient, "claim remote endpoint", fmt.Sprintf("replace stale endpoint claim %q", desired.Name), err)
	}
	return nil
}

func (c *KubernetesEndpointClaimer) Release(ctx context.Context, claim EndpointClaim) error {
	if err := c.validate(claim); err != nil {
		return err
	}
	existing := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: c.namespace(), Name: endpointClaimName(claim.EndpointName)}
	if err := c.reader().Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return contracts.WrapError(contracts.ErrorKindTransient, "release remote endpoint", fmt.Sprintf("read endpoint claim %q", key.Name), err)
	}
	if existing.Spec.HolderIdentity == nil || *existing.Spec.HolderIdentity != claim.HolderKey {
		return contracts.NewError(contracts.ErrorKindConflict, "release remote endpoint", fmt.Sprintf("endpoint %q is no longer held by allocation %s", claim.EndpointName, claim.HolderKey))
	}
	uid := existing.UID
	resourceVersion := existing.ResourceVersion
	if err := c.Client.Delete(ctx, existing, &client.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &resourceVersion,
	}}); err != nil && !apierrors.IsNotFound(err) {
		return contracts.WrapError(contracts.ErrorKindTransient, "release remote endpoint", fmt.Sprintf("delete endpoint claim %q", existing.Name), err)
	}
	return nil
}

func (c *KubernetesEndpointClaimer) claimObject(claim EndpointClaim) *coordinationv1.Lease {
	now := metav1.NewMicroTime(c.now())
	holder := claim.HolderKey
	duration := int32(claim.TTL / time.Second)
	if duration < 1 {
		duration = 1
	}
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      endpointClaimName(claim.EndpointName),
			Namespace: c.namespace(),
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":   "accelerator-control-plane",
				"accelerator.qcrs.io/claim-type": "remote-endpoint",
			},
			Annotations: map[string]string{
				claimEndpointAnnotation:       claim.EndpointName,
				claimLeaseNamespaceAnnotation: claim.LeaseNamespace,
				claimLeaseNameAnnotation:      claim.LeaseName,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			AcquireTime:          &now,
			RenewTime:            &now,
			LeaseDurationSeconds: &duration,
		},
	}
}

func (c *KubernetesEndpointClaimer) claimIsStale(ctx context.Context, claim *coordinationv1.Lease) (bool, error) {
	namespace := claim.Annotations[claimLeaseNamespaceAnnotation]
	name := claim.Annotations[claimLeaseNameAnnotation]
	if namespace == "" || name == "" || claim.Spec.HolderIdentity == nil || *claim.Spec.HolderIdentity == "" {
		return false, nil
	}
	lease := &acceleratorv1alpha1.AcceleratorLease{}
	if err := c.reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, contracts.WrapError(contracts.ErrorKindTransient, "claim remote endpoint", "validate existing claim owner", err)
	}
	if string(lease.UID) != *claim.Spec.HolderIdentity {
		return true, nil
	}
	switch lease.Status.Phase {
	case acceleratorv1alpha1.AllocationPhaseReleased,
		acceleratorv1alpha1.AllocationPhaseFailed,
		acceleratorv1alpha1.AllocationPhaseExpired:
		return true, nil
	default:
		return false, nil
	}
}

func (c *KubernetesEndpointClaimer) validate(claim EndpointClaim) error {
	if c == nil || c.Client == nil {
		return contracts.NewError(contracts.ErrorKindInvalid, "claim remote endpoint", "Kubernetes client is required")
	}
	if claim.EndpointName == "" || claim.HolderKey == "" || claim.LeaseNamespace == "" || claim.LeaseName == "" {
		return contracts.NewError(contracts.ErrorKindInvalid, "claim remote endpoint", "endpoint, holder, and allocation Lease identity are required")
	}
	return nil
}

func (c *KubernetesEndpointClaimer) reader() client.Reader {
	if c.APIReader != nil {
		return c.APIReader
	}
	return c.Client
}

func (c *KubernetesEndpointClaimer) namespace() string {
	if c.Namespace == "" {
		return defaultCoordinationNamespace
	}
	return c.Namespace
}

func (c *KubernetesEndpointClaimer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func endpointClaimName(endpointName string) string {
	sum := sha256.Sum256([]byte(endpointName))
	return endpointClaimPrefix + hex.EncodeToString(sum[:10])
}
