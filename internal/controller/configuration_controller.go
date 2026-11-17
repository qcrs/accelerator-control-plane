package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"github.com/qcrs/accelerator-control-plane/internal/inventory"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ConditionReady              = "Ready"
	ConditionConfigured         = "Configured"
	configurationResyncInterval = 5 * time.Minute
)

type ProviderReconciler struct {
	client.Client
	Providers *contracts.ProviderRegistry
	Recorder  record.EventRecorder
	Now       func() time.Time
}

type PoolReconciler struct {
	client.Client
	Providers *contracts.ProviderRegistry
	Recorder  record.EventRecorder
	Now       func() time.Time
}

type EndpointReconciler struct {
	client.Client
	Providers *contracts.ProviderRegistry
	Recorder  record.EventRecorder
	Now       func() time.Time
}

func (r *ProviderReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&acceleratorv1alpha1.AcceleratorProvider{}).
		Complete(r)
}

func (r *PoolReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&acceleratorv1alpha1.AcceleratorPool{}).
		Watches(&acceleratorv1alpha1.AcceleratorProvider{}, handler.EnqueueRequestsFromMapFunc(r.poolsForProvider)).
		Watches(&acceleratorv1alpha1.AcceleratorEndpoint{}, handler.EnqueueRequestsFromMapFunc(r.poolsForCapacityChange)).
		Watches(&acceleratorv1alpha1.AcceleratorLease{}, handler.EnqueueRequestsFromMapFunc(r.poolsForCapacityChange)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.poolsForCapacityChange)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.poolsForCapacityChange)).
		Complete(r)
}

func (r *EndpointReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&acceleratorv1alpha1.AcceleratorEndpoint{}).
		Watches(&acceleratorv1alpha1.AcceleratorPool{}, handler.EnqueueRequestsFromMapFunc(r.endpointsForPool)).
		Watches(&acceleratorv1alpha1.AcceleratorProvider{}, handler.EnqueueRequestsFromMapFunc(r.endpointsForProvider)).
		Watches(&acceleratorv1alpha1.AcceleratorLease{}, handler.EnqueueRequestsFromMapFunc(r.endpointsForLease)).
		Complete(r)
}

func (r *EndpointReconciler) endpointsForLease(ctx context.Context, obj client.Object) []reconcile.Request {
	lease, ok := obj.(*acceleratorv1alpha1.AcceleratorLease)
	if !ok {
		return nil
	}
	seen := map[string]struct{}{}
	requests := make([]reconcile.Request, 0, len(lease.Spec.Targets))
	for _, target := range lease.Spec.Targets {
		if target.EndpointName == "" {
			continue
		}
		if _, exists := seen[target.EndpointName]; exists {
			continue
		}
		seen[target.EndpointName] = struct{}{}
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: target.EndpointName}})
	}
	return requests
}

func (r *ProviderReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	provider := &acceleratorv1alpha1.AcceleratorProvider{}
	if err := r.Get(ctx, request.NamespacedName, provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := provider.DeepCopy().Status
	reason, message, err := validateProviderConfiguration(provider, r.Providers)
	status := metav1.ConditionTrue
	if err != nil {
		status = metav1.ConditionFalse
	}
	setCondition(&provider.Status.Conditions, provider.Generation, ConditionReady, status, reason, message, configNow(r.Now))
	provider.Status.ObservedGeneration = provider.Generation
	if reflect.DeepEqual(before, provider.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Update(ctx, provider); err != nil {
		return ctrl.Result{}, err
	}
	recordConfigurationTransition(r.Recorder, provider, ConditionReady, before.Conditions, provider.Status.Conditions)
	return ctrl.Result{}, nil
}

func (r *PoolReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	pool := &acceleratorv1alpha1.AcceleratorPool{}
	if err := r.Get(ctx, request.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := pool.DeepCopy().Status
	reason, message, err := validatePoolConfiguration(ctx, r.Client, pool, r.Providers)
	status := metav1.ConditionTrue
	if err != nil {
		status = metav1.ConditionFalse
		pool.Status.Capacity = acceleratorv1alpha1.CapacitySummary{}
		pool.Status.ReadyEndpoints = 0
	} else {
		provider := &acceleratorv1alpha1.AcceleratorProvider{}
		if getErr := r.Get(ctx, types.NamespacedName{Name: pool.Spec.ProviderRef.Name}, provider); getErr != nil {
			return ctrl.Result{}, getErr
		}
		capacity, readyEndpoints, capacityErr := inventory.SummarizePoolCapacity(ctx, r.Client, pool, provider.Spec.Driver, r.Providers)
		if capacityErr != nil {
			return ctrl.Result{}, capacityErr
		}
		pool.Status.Capacity = capacity
		pool.Status.ReadyEndpoints = readyEndpoints
	}
	setCondition(&pool.Status.Conditions, pool.Generation, ConditionReady, status, reason, message, configNow(r.Now))
	pool.Status.ObservedGeneration = pool.Generation
	if reflect.DeepEqual(before, pool.Status) {
		return ctrl.Result{RequeueAfter: configurationResyncInterval}, nil
	}
	if err := r.Status().Update(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	recordConfigurationTransition(r.Recorder, pool, ConditionReady, before.Conditions, pool.Status.Conditions)
	return ctrl.Result{RequeueAfter: configurationResyncInterval}, nil
}

func (r *EndpointReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{}
	if err := r.Get(ctx, request.NamespacedName, endpoint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := endpoint.DeepCopy().Status
	reason, message, err := validateEndpointConfiguration(ctx, r.Client, endpoint, r.Providers)
	status := metav1.ConditionTrue
	if err != nil {
		status = metav1.ConditionFalse
		endpoint.Status.Health = acceleratorv1alpha1.DeviceHealthUnknown
		endpoint.Status.Available = 0
	} else {
		// Lease accounting is the only authoritative occupancy signal available
		// to the control plane. A missing remote probe never implies idle.
		leases := &acceleratorv1alpha1.AcceleratorLeaseList{}
		if listErr := r.List(ctx, leases); listErr != nil {
			return ctrl.Result{}, listErr
		}
		reserved := int32(0)
		for i := range leases.Items {
			lease := &leases.Items[i]
			if lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseReserved && lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseBound {
				continue
			}
			for _, target := range lease.Spec.Targets {
				if target.EndpointName == endpoint.Name {
					reserved++
					break
				}
			}
		}
		endpoint.Status.Reserved = reserved
		endpoint.Status.Capacity = acceleratorv1alpha1.CapacitySummary{Total: int32(len(endpoint.Spec.Devices)), Reserved: reserved}
		endpoint.Status.Health = acceleratorv1alpha1.DeviceHealthUnknown
		endpoint.Status.Available = 0
		endpoint.Status.ObservationSource = "lease-accounting"
		endpoint.Status.Confidence = "Derived"
		endpoint.Status.Reason = "RemoteProbeUnavailable"
		endpoint.Status.Message = "remote health and active sessions are not observable; endpoint is not treated as idle"
	}
	setCondition(&endpoint.Status.Conditions, endpoint.Generation, ConditionConfigured, status, reason, message, configNow(r.Now))
	endpoint.Status.ObservedGeneration = endpoint.Generation
	if reflect.DeepEqual(before, endpoint.Status) {
		return ctrl.Result{RequeueAfter: configurationResyncInterval}, nil
	}
	if err := r.Status().Update(ctx, endpoint); err != nil {
		return ctrl.Result{}, err
	}
	recordConfigurationTransition(r.Recorder, endpoint, ConditionConfigured, before.Conditions, endpoint.Status.Conditions)
	return ctrl.Result{RequeueAfter: configurationResyncInterval}, nil
}

func validateProviderConfiguration(provider *acceleratorv1alpha1.AcceleratorProvider, registry *contracts.ProviderRegistry) (string, string, error) {
	if registry == nil {
		return "ControllerMisconfigured", "provider registry is not configured", fmt.Errorf("provider registry is nil")
	}
	implementation, err := registry.Get(provider.Spec.Driver)
	if err != nil {
		return "DriverNotRegistered", err.Error(), err
	}
	if len(provider.Spec.DeviceKinds) == 0 {
		err := fmt.Errorf("at least one device kind must be declared")
		return "InvalidConfiguration", err.Error(), err
	}
	capabilities := implementation.Capabilities()
	seen := map[acceleratorv1alpha1.DeviceKind]struct{}{}
	for _, kind := range provider.Spec.DeviceKinds {
		if _, duplicate := seen[kind]; duplicate {
			err := fmt.Errorf("device kind %q is duplicated", kind)
			return "InvalidConfiguration", err.Error(), err
		}
		seen[kind] = struct{}{}
		if !containsDeviceKind(capabilities.DeviceKinds, kind) {
			err := fmt.Errorf("driver %q does not support device kind %q", provider.Spec.Driver, kind)
			return "UnsupportedCapability", err.Error(), err
		}
	}
	if provider.Spec.TrustBundleSecretRef != nil && !capabilities.SupportsTLS {
		err := fmt.Errorf("driver %q does not support TLS trust bundles", provider.Spec.Driver)
		return "UnsupportedCapability", err.Error(), err
	}
	if provider.Spec.AuthSecretRef != nil && !capabilities.SupportsAuth {
		err := fmt.Errorf("driver %q does not support authentication secrets", provider.Spec.Driver)
		return "UnsupportedCapability", err.Error(), err
	}
	if validator, supported := implementation.(contracts.ProviderConfigurationValidator); supported {
		if err := validator.ValidateProvider(provider); err != nil {
			return configurationError(err)
		}
	}
	return "ConfigurationValid", fmt.Sprintf("driver %q is registered and provider configuration is valid", provider.Spec.Driver), nil
}

func validatePoolConfiguration(ctx context.Context, reader client.Reader, pool *acceleratorv1alpha1.AcceleratorPool, registry *contracts.ProviderRegistry) (string, string, error) {
	if pool.Spec.Unschedulable {
		err := fmt.Errorf("pool is administratively unschedulable")
		return "Unschedulable", err.Error(), err
	}
	providerObject := &acceleratorv1alpha1.AcceleratorProvider{}
	if err := reader.Get(ctx, types.NamespacedName{Name: pool.Spec.ProviderRef.Name}, providerObject); err != nil {
		if apierrors.IsNotFound(err) {
			return "ProviderNotFound", fmt.Sprintf("provider %q does not exist", pool.Spec.ProviderRef.Name), err
		}
		return "ReadFailed", err.Error(), err
	}
	if reason, message, err := validateProviderConfiguration(providerObject, registry); err != nil {
		return reason, fmt.Sprintf("provider %s is invalid: %s", providerObject.Name, message), err
	}
	implementation, err := registry.Get(providerObject.Spec.Driver)
	if err != nil {
		return "DriverNotRegistered", err.Error(), err
	}
	capabilities := implementation.Capabilities()
	if !containsExecutionMode(capabilities.Modes, pool.Spec.Location) {
		err := fmt.Errorf("driver %q does not support %s pools", providerObject.Spec.Driver, pool.Spec.Location)
		return "UnsupportedCapability", err.Error(), err
	}
	if !containsDeviceKind(providerObject.Spec.DeviceKinds, pool.Spec.DeviceType) {
		err := fmt.Errorf("provider %q does not declare device kind %q", providerObject.Name, pool.Spec.DeviceType)
		return "UnsupportedCapability", err.Error(), err
	}
	if pool.Spec.Remote != nil {
		allocationMode := pool.Spec.Remote.AllocationMode
		if allocationMode == "" {
			allocationMode = acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint
		}
		if !containsRemoteAllocationMode(capabilities.RemoteAllocationModes, allocationMode) {
			err := fmt.Errorf("driver %q does not support remote allocation mode %q", providerObject.Spec.Driver, allocationMode)
			return "UnsupportedCapability", err.Error(), err
		}
	}
	if pool.Spec.Local != nil {
		resourceForm := pool.Spec.Local.ResourceForm
		if resourceForm == "" {
			resourceForm = acceleratorv1alpha1.ResourceFormCustom
		}
		if !containsResourceForm(capabilities.ResourceForms, resourceForm) {
			err := fmt.Errorf("driver %q does not support local resource form %q", providerObject.Spec.Driver, resourceForm)
			return "UnsupportedCapability", err.Error(), err
		}
	}
	if err := implementation.Validate(providerObject, pool); err != nil {
		return configurationError(err)
	}
	if pool.Spec.Remote != nil {
		if _, err := metav1.LabelSelectorAsSelector(&pool.Spec.Remote.EndpointSelector); err != nil {
			return "InvalidSelector", err.Error(), err
		}
	}
	return "ConfigurationValid", fmt.Sprintf("pool is compatible with provider %q", providerObject.Name), nil
}

func validateEndpointConfiguration(ctx context.Context, reader client.Reader, endpoint *acceleratorv1alpha1.AcceleratorEndpoint, registry *contracts.ProviderRegistry) (string, string, error) {
	if endpoint.Spec.Unschedulable {
		err := fmt.Errorf("endpoint is administratively unschedulable")
		return "Unschedulable", err.Error(), err
	}
	pool := &acceleratorv1alpha1.AcceleratorPool{}
	if err := reader.Get(ctx, types.NamespacedName{Name: endpoint.Spec.PoolRef.Name}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return "PoolNotFound", fmt.Sprintf("pool %q does not exist", endpoint.Spec.PoolRef.Name), err
		}
		return "ReadFailed", err.Error(), err
	}
	if pool.Spec.Location != acceleratorv1alpha1.ExecutionModeRemote || pool.Spec.Remote == nil {
		err := fmt.Errorf("pool %q is not a remote pool", pool.Name)
		return "InvalidPool", err.Error(), err
	}
	if reason, message, err := validatePoolConfiguration(ctx, reader, pool, registry); err != nil {
		return reason, fmt.Sprintf("pool %s is invalid: %s", pool.Name, message), err
	}
	providerObject := &acceleratorv1alpha1.AcceleratorProvider{}
	if err := reader.Get(ctx, types.NamespacedName{Name: pool.Spec.ProviderRef.Name}, providerObject); err != nil {
		return "ProviderNotFound", err.Error(), err
	}
	if endpoint.Spec.Runtime.Driver != providerObject.Spec.Driver {
		err := fmt.Errorf("runtime driver %q does not match provider driver %q", endpoint.Spec.Runtime.Driver, providerObject.Spec.Driver)
		return "DriverMismatch", err.Error(), err
	}
	implementation, err := registry.Get(providerObject.Spec.Driver)
	if err != nil {
		return "DriverNotRegistered", err.Error(), err
	}
	capabilities := implementation.Capabilities()
	if len(endpoint.Spec.Devices) == 0 {
		err := fmt.Errorf("endpoint must declare at least one device")
		return "InvalidDevices", err.Error(), err
	}
	ids := map[string]struct{}{}
	uuids := map[string]struct{}{}
	for _, device := range endpoint.Spec.Devices {
		if device.ID == "" {
			err := fmt.Errorf("device ID must not be empty")
			return "InvalidDevices", err.Error(), err
		}
		if _, duplicate := ids[device.ID]; duplicate {
			err := fmt.Errorf("device ID %q is duplicated", device.ID)
			return "InvalidDevices", err.Error(), err
		}
		ids[device.ID] = struct{}{}
		resourceForm := device.ResourceForm
		if resourceForm == "" {
			resourceForm = acceleratorv1alpha1.ResourceFormWholeDevice
		}
		if !containsResourceForm(capabilities.ResourceForms, resourceForm) {
			err := fmt.Errorf("driver %q does not support endpoint device resource form %q", providerObject.Spec.Driver, resourceForm)
			return "UnsupportedCapability", err.Error(), err
		}
		if device.UUID != "" {
			if _, duplicate := uuids[device.UUID]; duplicate {
				err := fmt.Errorf("device UUID %q is duplicated", device.UUID)
				return "InvalidDevices", err.Error(), err
			}
			uuids[device.UUID] = struct{}{}
		}
	}
	allEndpoints := &acceleratorv1alpha1.AcceleratorEndpointList{}
	if err := reader.List(ctx, allEndpoints); err != nil {
		return "ReadFailed", err.Error(), err
	}
	for index := range allEndpoints.Items {
		other := &allEndpoints.Items[index]
		if other.Name == endpoint.Name {
			continue
		}
		for _, device := range other.Spec.Devices {
			if device.UUID == "" {
				continue
			}
			if _, duplicate := uuids[device.UUID]; duplicate {
				err := fmt.Errorf("device UUID %q is also declared by endpoint %q", device.UUID, other.Name)
				return "DuplicateDevice", err.Error(), err
			}
		}
	}
	remote := contracts.RemoteEndpoint{
		Name:          endpoint.Name,
		PoolName:      endpoint.Spec.PoolRef.Name,
		Address:       endpoint.Spec.Address,
		Runtime:       endpoint.Spec.Runtime,
		Devices:       endpoint.Spec.Devices,
		Parameters:    endpoint.Spec.Parameters,
		Unschedulable: endpoint.Spec.Unschedulable,
	}
	if err := registry.ValidateEndpoint(remote); err != nil {
		return configurationError(err)
	}
	return "ConfigurationValid", fmt.Sprintf("endpoint exposes %d statically validated device(s)", len(endpoint.Spec.Devices)), nil
}

func configurationError(err error) (string, string, error) {
	switch {
	case contracts.IsErrorKind(err, contracts.ErrorKindUnsupported):
		return "UnsupportedCapability", err.Error(), err
	case contracts.IsErrorKind(err, contracts.ErrorKindInvalid):
		return "InvalidConfiguration", err.Error(), err
	default:
		return "ValidationFailed", err.Error(), err
	}
}

func containsDeviceKind(kinds []acceleratorv1alpha1.DeviceKind, wanted acceleratorv1alpha1.DeviceKind) bool {
	for _, kind := range kinds {
		if kind == wanted {
			return true
		}
	}
	return false
}

func containsExecutionMode(modes []acceleratorv1alpha1.ExecutionMode, wanted acceleratorv1alpha1.ExecutionMode) bool {
	for _, mode := range modes {
		if mode == wanted {
			return true
		}
	}
	return false
}

func containsRemoteAllocationMode(modes []acceleratorv1alpha1.RemoteAllocationMode, wanted acceleratorv1alpha1.RemoteAllocationMode) bool {
	for _, mode := range modes {
		if mode == wanted {
			return true
		}
	}
	return false
}

func containsResourceForm(forms []acceleratorv1alpha1.ResourceForm, wanted acceleratorv1alpha1.ResourceForm) bool {
	for _, form := range forms {
		if form == wanted {
			return true
		}
	}
	return false
}

func configNow(clock func() time.Time) time.Time {
	if clock == nil {
		return time.Now()
	}
	return clock()
}

func recordConfigurationTransition(recorder record.EventRecorder, object runtime.Object, conditionType string, before, after []metav1.Condition) {
	if recorder == nil {
		return
	}
	oldReady := findMetaCondition(before, conditionType)
	newReady := findMetaCondition(after, conditionType)
	if newReady == nil || (oldReady != nil && oldReady.Status == newReady.Status && oldReady.Reason == newReady.Reason && oldReady.Message == newReady.Message) {
		return
	}
	eventType := corev1.EventTypeNormal
	eventReason := "ConfigurationReady"
	if newReady.Status != metav1.ConditionTrue {
		eventType = corev1.EventTypeWarning
		eventReason = "ConfigurationInvalid"
	}
	recorder.Event(object, eventType, eventReason, newReady.Message)
}

func findMetaCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}

func (r *PoolReconciler) poolsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	pools := &acceleratorv1alpha1.AcceleratorPoolList{}
	if err := r.List(ctx, pools); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for index := range pools.Items {
		if pools.Items[index].Spec.ProviderRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: pools.Items[index].Name}})
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Name < requests[j].Name })
	return requests
}

func (r *PoolReconciler) poolsForCapacityChange(ctx context.Context, _ client.Object) []reconcile.Request {
	pools := &acceleratorv1alpha1.AcceleratorPoolList{}
	if err := r.List(ctx, pools); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(pools.Items))
	for index := range pools.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: pools.Items[index].Name}})
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Name < requests[j].Name })
	return requests
}

func (r *EndpointReconciler) endpointsForPool(ctx context.Context, object client.Object) []reconcile.Request {
	return r.endpointsForPools(ctx, map[string]struct{}{object.GetName(): struct{}{}})
}

func (r *EndpointReconciler) endpointsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	pools := &acceleratorv1alpha1.AcceleratorPoolList{}
	if err := r.List(ctx, pools); err != nil {
		return nil
	}
	names := map[string]struct{}{}
	for index := range pools.Items {
		if pools.Items[index].Spec.ProviderRef.Name == object.GetName() {
			names[pools.Items[index].Name] = struct{}{}
		}
	}
	return r.endpointsForPools(ctx, names)
}

func (r *EndpointReconciler) endpointsForPools(ctx context.Context, poolNames map[string]struct{}) []reconcile.Request {
	endpoints := &acceleratorv1alpha1.AcceleratorEndpointList{}
	if err := r.List(ctx, endpoints); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for index := range endpoints.Items {
		if _, found := poolNames[endpoints.Items[index].Spec.PoolRef.Name]; found {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: endpoints.Items[index].Name}})
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Name < requests[j].Name })
	return requests
}
