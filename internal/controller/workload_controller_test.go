package controller

import (
	"context"
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	executionadapter "github.com/qcrs/accelerator-control-plane/internal/adapter"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"github.com/qcrs/accelerator-control-plane/internal/injector"
	"github.com/qcrs/accelerator-control-plane/internal/inventory"
	"github.com/qcrs/accelerator-control-plane/internal/policy"
	cricketprovider "github.com/qcrs/accelerator-control-plane/internal/provider/cricket"
	localprovider "github.com/qcrs/accelerator-control-plane/internal/provider/local"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWorkloadReconcilerLocalLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := acceleratorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add accelerator scheme: %v", err)
	}

	provider := &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver: localprovider.DriverName, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
		},
	}
	pool := &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "local"},
			Location:    acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Local: &acceleratorv1alpha1.LocalPoolConfig{ResourceName: "nvidia.com/gpu"},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubernetes.io/hostname": "node-a"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	workload := &acceleratorv1alpha1.AcceleratorWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "local-lifecycle", Namespace: "default", UID: types.UID("workload-uid-1234"), Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorWorkloadSpec{
			Accelerator: acceleratorv1alpha1.AcceleratorRequest{DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: 1},
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "worker", Image: "example.test/workload:latest"}},
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(
			&acceleratorv1alpha1.AcceleratorWorkload{}, &acceleratorv1alpha1.AcceleratorLease{},
			&acceleratorv1alpha1.AcceleratorProvider{}, &acceleratorv1alpha1.AcceleratorPool{},
			&acceleratorv1alpha1.AcceleratorEndpoint{}, &corev1.Pod{},
		).
		WithObjects(provider, pool, node, workload).
		Build()
	adapters, err := executionadapter.NewRegistry(executionadapter.NewBinding(
		localprovider.New(),
		&allocation.LocalAllocator{Now: func() time.Time { return time.Unix(100, 0) }},
	))
	if err != nil {
		t.Fatalf("create adapter registry: %v", err)
	}
	registry := adapters.Providers()
	reconciler := &WorkloadReconciler{
		Client: kubeClient, Scheme: scheme,
		Inventory: inventory.NewKubernetesReader(kubeClient, registry),
		Policy:    policy.NewLocalFirst(),
		Adapters:  adapters,
		Injector:  injector.NewPodMutator(),
		Now:       func() time.Time { return time.Unix(100, 0) },
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: workload.Name}}

	current := &acceleratorv1alpha1.AcceleratorWorkload{}
	for attempt := 0; attempt < 10; attempt++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("reconcile step %d: %v", attempt, err)
		}
		if err := kubeClient.Get(ctx, request.NamespacedName, current); err != nil {
			t.Fatalf("read workload after step %d: %v", attempt, err)
		}
		if current.Status.Phase == acceleratorv1alpha1.WorkloadPhaseStarting {
			break
		}
	}
	if current.Status.Phase != acceleratorv1alpha1.WorkloadPhaseStarting || current.Status.PodRef == nil {
		t.Fatalf("workload did not reach Starting: %#v", current.Status)
	}
	pod := &corev1.Pod{}
	podKey := client.ObjectKey{Namespace: "default", Name: current.Status.PodRef.Name}
	if err := kubeClient.Get(ctx, podKey, pod); err != nil {
		t.Fatalf("read generated Pod: %v", err)
	}
	if got := pod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; got.Value() != 1 {
		t.Fatalf("expected one injected GPU, got %s", got.String())
	}
	if !conditionIsTrue(current.Status.Conditions, ConditionRuntimeReady) {
		t.Fatalf("runtime materialization was not reported ready: %#v", current.Status.Conditions)
	}
	boundLease := &acceleratorv1alpha1.AcceleratorLease{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: current.Status.AllocationRef}, boundLease); err != nil {
		t.Fatalf("read bound Lease: %v", err)
	}
	if !conditionIsTrue(boundLease.Status.Conditions, ConditionRuntimeReady) {
		t.Fatalf("Lease runtime materialization was not reported ready: %#v", boundLease.Status.Conditions)
	}
	pod.Spec.NodeName = "node-a"
	if err := kubeClient.Update(ctx, pod); err != nil {
		t.Fatalf("bind fake Pod to node: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := kubeClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("mark fake Pod Ready: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile Ready Pod: %v", err)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatalf("read running workload: %v", err)
	}
	if current.Status.Phase != acceleratorv1alpha1.WorkloadPhaseRunning || !current.Status.Fallback.ReadyObserved {
		t.Fatalf("workload did not become Running: %#v", current.Status)
	}
	pod.Status.Phase = corev1.PodSucceeded
	if err := kubeClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("mark fake Pod Succeeded: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile completed Pod: %v", err)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatalf("read completed workload: %v", err)
	}
	if current.Status.Phase != acceleratorv1alpha1.WorkloadPhaseSucceeded {
		t.Fatalf("workload did not become Succeeded: %#v", current.Status)
	}
	lease := &acceleratorv1alpha1.AcceleratorLease{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: current.Status.AllocationRef}, lease); err != nil {
		t.Fatalf("read released Lease: %v", err)
	}
	if lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseReleased {
		t.Fatalf("expected released Lease, got %s", lease.Status.Phase)
	}
}

func TestFallbackFailureDetectionAndTargetExclusion(t *testing.T) {
	now := time.Unix(200, 0)
	pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "selected node has no capacity",
	}}}}
	failure, found := detectStartupFailure(pod, now.Add(-time.Minute), now, 5*time.Minute)
	if !found || failure.Reason != "Unschedulable" {
		t.Fatalf("unexpected startup failure: %#v, found=%t", failure, found)
	}
	workload := &acceleratorv1alpha1.AcceleratorWorkload{Status: acceleratorv1alpha1.AcceleratorWorkloadStatus{
		Fallback: acceleratorv1alpha1.FallbackStatus{History: []acceleratorv1alpha1.StartupFailureStatus{{
			Mode:    acceleratorv1alpha1.ExecutionModeLocal,
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "local"},
			Targets: []acceleratorv1alpha1.AllocationTarget{{NodeName: "node-a"}},
		}}},
	}}
	preferred, exclusions := fallbackSelectionContext(workload)
	if preferred != acceleratorv1alpha1.ExecutionModeRemote || len(exclusions) != 1 || exclusions[0].TargetName != "node-a" {
		t.Fatalf("unexpected fallback selection context: preferred=%s exclusions=%#v", preferred, exclusions)
	}
}

func TestStartupFailureOnlyImmediatelyRetriesManagedRuntimeSetup(t *testing.T) {
	now := time.Unix(300, 0)
	applicationImageFailure := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: "worker", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}}}
	if failure, found := detectStartupFailure(applicationImageFailure, now, now, time.Minute); found {
		t.Fatalf("application image failure must not trigger immediate cross-target fallback: %#v", failure)
	}

	runtimeImageFailure := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			contracts.RuntimeInitContainersAnnotation: "accelerator-runtime-setup",
		}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
			Name: "accelerator-runtime-setup", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}},
	}
	failure, found := detectStartupFailure(runtimeImageFailure, now, now, time.Minute)
	if !found || failure.Reason != "ImagePullBackOff" {
		t.Fatalf("managed runtime setup failure should be immediately retryable: %#v, found=%t", failure, found)
	}
}

func TestWorkloadReconcilerFallsBackFromLocalToRemote(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := acceleratorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add accelerator scheme: %v", err)
	}
	localProvider := &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver: localprovider.DriverName, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
		},
	}
	remoteProvider := &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "remote", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver: cricketprovider.DriverName, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
			ClientRuntime: &acceleratorv1alpha1.ClientRuntimeSpec{
				Image: "example.test/cricket-client:test", RuntimeVersion: "cricket-test-version",
			},
		},
	}
	localPool := &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "local"},
			Location:    acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Priority: 100, Local: &acceleratorv1alpha1.LocalPoolConfig{ResourceName: "nvidia.com/gpu"},
		},
	}
	remotePool := &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "remote", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "remote"},
			Location:    acceleratorv1alpha1.ExecutionModeRemote, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Remote: &acceleratorv1alpha1.RemotePoolConfig{AllocationMode: acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubernetes.io/hostname": "node-a"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint-a", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "remote"}, Address: "gpu-a.example",
			Runtime:    acceleratorv1alpha1.RuntimeInfo{Driver: cricketprovider.DriverName, Version: "cricket-test-version", APIVersion: "1"},
			Parameters: map[string]string{cricketprovider.ParameterRPCID: "1"},
			Devices: []acceleratorv1alpha1.DeviceInfo{{
				ID: "gpu-0", Vendor: "nvidia", Model: "A100", Memory: resource.MustParse("40Gi"),
				Health: acceleratorv1alpha1.DeviceHealthHealthy,
			}},
		},
	}
	workload := &acceleratorv1alpha1.AcceleratorWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "fallback", Namespace: "default", UID: types.UID("fallback-workload-uid"), Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorWorkloadSpec{
			Accelerator: acceleratorv1alpha1.AcceleratorRequest{DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: 1},
			Placement: acceleratorv1alpha1.PlacementPolicy{
				Mode:  acceleratorv1alpha1.PlacementModeAuto,
				Order: []acceleratorv1alpha1.ExecutionMode{acceleratorv1alpha1.ExecutionModeLocal, acceleratorv1alpha1.ExecutionModeRemote},
				Fallback: &acceleratorv1alpha1.FallbackPolicy{
					Enabled: true, MaxAttempts: 2,
					StartupTimeout: metav1.Duration{Duration: time.Minute}, Backoff: metav1.Duration{Duration: time.Second},
				},
			},
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "worker", Image: "example.test/workload:latest"}},
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(
			&acceleratorv1alpha1.AcceleratorWorkload{}, &acceleratorv1alpha1.AcceleratorLease{},
			&acceleratorv1alpha1.AcceleratorProvider{}, &acceleratorv1alpha1.AcceleratorPool{},
			&acceleratorv1alpha1.AcceleratorEndpoint{}, &corev1.Pod{},
		).
		WithObjects(localProvider, remoteProvider, localPool, remotePool, node, endpoint, workload).
		Build()
	now := time.Unix(1000, 0)
	claimer := &controllerEndpointClaimer{}
	remoteAllocator := &allocation.RemoteAllocator{
		Resolver: &allocation.KubernetesEndpointResolver{Client: kubeClient}, Claimer: claimer,
		Now: func() time.Time { return now },
	}
	adapters, err := executionadapter.NewRegistry(
		executionadapter.NewBinding(localprovider.New(), &allocation.LocalAllocator{Now: func() time.Time { return now }}),
		executionadapter.NewBinding(cricketprovider.New(), remoteAllocator),
	)
	if err != nil {
		t.Fatalf("create execution adapter registry: %v", err)
	}
	registry := adapters.Providers()
	remoteAllocator.Validator = registry
	reconciler := &WorkloadReconciler{
		Client: kubeClient, Scheme: scheme,
		Inventory: inventory.NewKubernetesReader(kubeClient, registry), Policy: policy.NewLocalFirst(),
		Adapters: adapters,
		Injector: injector.NewPodMutator(), Now: func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: workload.Name}}
	current := &acceleratorv1alpha1.AcceleratorWorkload{}
	for step := 0; step < 10; step++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("initial reconcile step %d: %v", step, err)
		}
		if err := kubeClient.Get(ctx, request.NamespacedName, current); err != nil {
			t.Fatalf("read initial workload: %v", err)
		}
		if current.Status.Phase == acceleratorv1alpha1.WorkloadPhaseStarting {
			break
		}
	}
	if current.Status.SelectedMode != acceleratorv1alpha1.ExecutionModeLocal || current.Status.PodRef == nil {
		t.Fatalf("expected initial local attempt, got %#v", current.Status)
	}
	failedPod := &corev1.Pod{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: current.Status.PodRef.Name}, failedPod); err != nil {
		t.Fatalf("read local Pod: %v", err)
	}
	failedPod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "selected local node became unavailable",
	}}
	if err := kubeClient.Status().Update(ctx, failedPod); err != nil {
		t.Fatalf("mark local Pod unschedulable: %v", err)
	}
	for step := 0; step < 20; step++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("fallback reconcile step %d: %v", step, err)
		}
		if err := kubeClient.Get(ctx, request.NamespacedName, current); err != nil {
			t.Fatalf("read fallback workload: %v", err)
		}
		if current.Status.Fallback.RequeueAt != nil && now.Before(current.Status.Fallback.RequeueAt.Time) {
			now = current.Status.Fallback.RequeueAt.Time.Add(time.Millisecond)
		}
		// The fake client does not run API-server object admission, so newly
		// created objects do not receive a UID. Remote endpoint claims use the
		// Lease UID as their fencing token; emulate that server-side field here.
		if current.Status.SelectedMode == acceleratorv1alpha1.ExecutionModeRemote && current.Status.AllocationRef != "" {
			remoteLease := &acceleratorv1alpha1.AcceleratorLease{}
			leaseKey := client.ObjectKey{Namespace: workload.Namespace, Name: current.Status.AllocationRef}
			if err := kubeClient.Get(ctx, leaseKey, remoteLease); err == nil && remoteLease.UID == "" {
				remoteLease.UID = types.UID("remote-lease-uid")
				if err := kubeClient.Update(ctx, remoteLease); err != nil {
					t.Fatalf("emulate Lease UID assignment: %v", err)
				}
			}
		}
		if current.Status.SelectedMode == acceleratorv1alpha1.ExecutionModeRemote && current.Status.Phase == acceleratorv1alpha1.WorkloadPhaseStarting {
			break
		}
	}
	if current.Status.SelectedMode != acceleratorv1alpha1.ExecutionModeRemote || current.Status.Phase != acceleratorv1alpha1.WorkloadPhaseStarting {
		t.Fatalf("workload did not fall back to Remote: %#v", current.Status)
	}
	if current.Status.Fallback.FailedAttempts != 1 || len(current.Status.Fallback.History) != 1 {
		t.Fatalf("fallback history was not persisted: %#v", current.Status.Fallback)
	}
	if holder := claimer.holders["endpoint-a"]; holder == "" {
		t.Fatal("remote fallback did not acquire the endpoint claim")
	}
	remotePod := &corev1.Pod{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: current.Status.PodRef.Name}, remotePod); err != nil {
		t.Fatalf("read remote fallback Pod: %v", err)
	}
	environment := map[string]string{}
	for _, variable := range remotePod.Spec.Containers[0].Env {
		environment[variable.Name] = variable.Value
	}
	if environment["REMOTE_GPU_ADDRESS"] != "gpu-a.example" || environment["LD_PRELOAD"] == "" {
		t.Fatalf("remote runtime was not injected: %#v", environment)
	}
}

func TestPermanentPodCreateErrorClassification(t *testing.T) {
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "", Kind: "Pod"}, "pod", nil)
	if !isPermanentPodCreateError(invalid) {
		t.Fatal("Invalid Pod must be classified as a permanent create rejection")
	}
	timeout := apierrors.NewTimeoutError("request timed out", 1)
	if isPermanentPodCreateError(timeout) {
		t.Fatal("timeout has an unknown create result and must retain the reservation")
	}
}

func TestChildNamesIncludeWorkloadIdentity(t *testing.T) {
	longPrefix := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	left := leaseName(longPrefix+"-left", "11111111-aaaa-bbbb-cccc-dddddddddddd", 1)
	right := leaseName(longPrefix+"-right", "22222222-aaaa-bbbb-cccc-dddddddddddd", 1)
	if left == right {
		t.Fatalf("distinct Workloads produced colliding Lease name %q", left)
	}
	if len(left) > 63 || len(right) > 63 {
		t.Fatalf("child names exceed DNS label length: %q, %q", left, right)
	}
}

type controllerEndpointClaimer struct {
	holders map[string]string
}

func conditionIsTrue(conditions []metav1.Condition, conditionType string) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == metav1.ConditionTrue
		}
	}
	return false
}

func (c *controllerEndpointClaimer) Claim(_ context.Context, claim allocation.EndpointClaim) error {
	if c.holders == nil {
		c.holders = map[string]string{}
	}
	if holder := c.holders[claim.EndpointName]; holder != "" && holder != claim.HolderKey {
		return contracts.NewError(contracts.ErrorKindConflict, "test claim", "endpoint already claimed")
	}
	c.holders[claim.EndpointName] = claim.HolderKey
	return nil
}

func (c *controllerEndpointClaimer) Release(_ context.Context, claim allocation.EndpointClaim) error {
	holder := c.holders[claim.EndpointName]
	if holder == "" {
		return nil
	}
	if holder != claim.HolderKey {
		return contracts.NewError(contracts.ErrorKindConflict, "test release", "endpoint is held by another allocation")
	}
	delete(c.holders, claim.EndpointName)
	return nil
}
