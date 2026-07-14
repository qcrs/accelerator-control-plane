package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultHeartbeatInterval = 15 * time.Second
	defaultLeaseDuration     = 45 * time.Second
)

type Config struct {
	EndpointName      string
	PoolName          string
	AgentID           string
	ControlAddress    string
	SystemNamespace   string
	HeartbeatInterval time.Duration
	LeaseDuration     time.Duration
}

func (c *Config) Default() {
	if c.SystemNamespace == "" {
		c.SystemNamespace = "accelerator-system"
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = defaultHeartbeatInterval
	}
	if c.LeaseDuration == 0 {
		c.LeaseDuration = defaultLeaseDuration
	}
}

func (c Config) Validate() error {
	if c.EndpointName == "" || c.PoolName == "" || c.AgentID == "" || c.ControlAddress == "" {
		return fmt.Errorf("endpoint name, pool name, Agent ID, and control address are required")
	}
	if c.HeartbeatInterval <= 0 || c.LeaseDuration <= 0 || c.HeartbeatInterval >= c.LeaseDuration {
		return fmt.Errorf("heartbeat interval must be positive and shorter than the Lease duration")
	}
	return nil
}

type Runner struct {
	Client  client.Client
	Probe   contracts.DeviceProbe
	Config  Config
	Now     func() time.Time
	OnError func(error)
}

func (r *Runner) Run(ctx context.Context) error {
	if r.Client == nil || r.Probe == nil {
		return fmt.Errorf("Agent client and device probe are required")
	}
	r.Config.Default()
	if err := r.Config.Validate(); err != nil {
		return err
	}
	r.reportError(r.reconcile(ctx))
	ticker := time.NewTicker(r.Config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reportError(r.reconcile(ctx))
		}
	}
}

func (r *Runner) reportError(err error) {
	if err != nil && r.OnError != nil {
		r.OnError(err)
	}
}

func (r *Runner) reconcile(ctx context.Context) error {
	endpoint, err := r.ensureEndpoint(ctx)
	if err != nil {
		return err
	}
	if err := r.renewHeartbeat(ctx); err != nil {
		return err
	}
	return r.reportInventory(ctx, endpoint)
}

func (r *Runner) ensureEndpoint(ctx context.Context) (*acceleratorv1alpha1.AcceleratorEndpoint, error) {
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{}
	key := types.NamespacedName{Name: r.Config.EndpointName}
	err := r.Client.Get(ctx, key, endpoint)
	if apierrors.IsNotFound(err) {
		endpoint = &acceleratorv1alpha1.AcceleratorEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: r.Config.EndpointName},
			Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
				PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: r.Config.PoolName},
			},
		}
		if err := r.Client.Create(ctx, endpoint); err != nil {
			return nil, err
		}
		return endpoint, nil
	}
	if err != nil {
		return nil, err
	}
	if endpoint.Spec.PoolRef.Name != r.Config.PoolName {
		return nil, fmt.Errorf("endpoint %q belongs to pool %q, not %q", endpoint.Name, endpoint.Spec.PoolRef.Name, r.Config.PoolName)
	}
	return endpoint, nil
}

func (r *Runner) renewHeartbeat(ctx context.Context) error {
	now := metav1.NewMicroTime(r.now())
	duration := int32(r.Config.LeaseDuration / time.Second)
	holder := r.Config.AgentID
	leaseName := heartbeatName(r.Config.EndpointName)
	key := types.NamespacedName{Namespace: r.Config.SystemNamespace, Name: leaseName}
	heartbeat := &coordinationv1.Lease{}
	err := r.Client.Get(ctx, key, heartbeat)
	if apierrors.IsNotFound(err) {
		heartbeat = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: r.Config.SystemNamespace},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &duration,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		return r.Client.Create(ctx, heartbeat)
	}
	if err != nil {
		return err
	}
	if heartbeat.Spec.HolderIdentity != nil && *heartbeat.Spec.HolderIdentity != r.Config.AgentID {
		return fmt.Errorf("heartbeat Lease %s/%s is held by %q", heartbeat.Namespace, heartbeat.Name, *heartbeat.Spec.HolderIdentity)
	}
	heartbeat.Spec.HolderIdentity = &holder
	heartbeat.Spec.LeaseDurationSeconds = &duration
	heartbeat.Spec.RenewTime = &now
	return r.Client.Update(ctx, heartbeat)
}

func (r *Runner) reportInventory(ctx context.Context, endpoint *acceleratorv1alpha1.AcceleratorEndpoint) error {
	devices, runtimeInfo, probeErr := r.Probe.Probe(ctx)
	latest := &acceleratorv1alpha1.AcceleratorEndpoint{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: endpoint.Name}, latest); err != nil {
		return err
	}
	now := metav1.NewTime(r.now())
	latest.Status.ObservedGeneration = latest.Generation
	latest.Status.AgentID = r.Config.AgentID
	latest.Status.ControlAddress = r.Config.ControlAddress
	latest.Status.HeartbeatLeaseRef = &acceleratorv1alpha1.HeartbeatLeaseReference{
		Namespace: r.Config.SystemNamespace,
		Name:      heartbeatName(r.Config.EndpointName),
	}
	setEndpointCondition(&latest.Status.Conditions, latest.Generation, "AgentReady", metav1.ConditionTrue, "HeartbeatRenewed", "Agent heartbeat is current", now)
	if probeErr != nil {
		setEndpointCondition(&latest.Status.Conditions, latest.Generation, "InventoryReady", metav1.ConditionFalse, "ProbeFailed", probeErr.Error(), now)
	} else {
		latest.Status.Runtime = runtimeInfo
		latest.Status.Devices = devices
		latest.Status.LastInventoryTime = &now
		setEndpointCondition(&latest.Status.Conditions, latest.Generation, "InventoryReady", metav1.ConditionTrue, "Reported", "device inventory was reported", now)
	}
	return r.Client.Status().Update(ctx, latest)
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func heartbeatName(endpointName string) string {
	const prefix = "agent-"
	maximum := 63 - len(prefix)
	name := strings.Trim(endpointName, "-")
	if len(name) > maximum {
		name = strings.TrimRight(name[:maximum], "-")
	}
	return prefix + name
}

func setEndpointCondition(conditions *[]metav1.Condition, generation int64, conditionType string, status metav1.ConditionStatus, reason, message string, now metav1.Time) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}
