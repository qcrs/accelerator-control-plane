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
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestWorkloadReconcilerFallsBackFromRemoteRuntimeInitToLocal(t *testing.T) {
	harness := newControllerHarness(t, "remote-to-local", []acceleratorv1alpha1.ExecutionMode{
		acceleratorv1alpha1.ExecutionModeRemote,
		acceleratorv1alpha1.ExecutionModeLocal,
	}, nil)
	workload := harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
	pod := harness.pod(t, workload)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "accelerator-runtime-setup",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ImagePullBackOff", Message: "runtime image is unavailable",
		}},
	}}
	if err := harness.client.Status().Update(harness.ctx, pod); err != nil {
		t.Fatalf("mark runtime init failure: %v", err)
	}

	workload = harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeLocal)
	if workload.Status.Fallback.FailedAttempts != 1 || len(workload.Status.Fallback.History) != 1 ||
		workload.Status.Fallback.History[0].Reason != "ImagePullBackOff" {
		t.Fatalf("runtime failure was not persisted: %#v", workload.Status.Fallback)
	}
	if holder := harness.claimer.holders["endpoint-a"]; holder != "" {
		t.Fatalf("remote claim remained held after fallback: %q", holder)
	}
	localPod := harness.pod(t, workload)
	if got := localPod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; got.Value() != 1 {
		t.Fatalf("local fallback did not request the local resource: %s", got.String())
	}
	for _, variable := range localPod.Spec.Containers[0].Env {
		if variable.Name == "REMOTE_GPU_ADDRESS" || variable.Name == "LD_PRELOAD" {
			t.Fatalf("local fallback retained remote runtime variable %#v", variable)
		}
	}
	if len(localPod.Spec.InitContainers) != 0 {
		t.Fatalf("local fallback retained remote init containers: %#v", localPod.Spec.InitContainers)
	}
}

func TestWorkloadReconcilerNeverFallsBackAfterReady(t *testing.T) {
	harness := newControllerHarness(t, "ready-boundary", []acceleratorv1alpha1.ExecutionMode{
		acceleratorv1alpha1.ExecutionModeRemote,
		acceleratorv1alpha1.ExecutionModeLocal,
	}, nil)
	workload := harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
	pod := harness.pod(t, workload)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := harness.client.Status().Update(harness.ctx, pod); err != nil {
		t.Fatalf("mark Pod Ready: %v", err)
	}
	harness.reconcile(t)
	workload = harness.workload(t)
	if !workload.Status.Fallback.ReadyObserved {
		t.Fatalf("Ready boundary was not persisted: %#v", workload.Status.Fallback)
	}
	originalLease := workload.Status.AllocationRef

	pod = harness.pod(t, workload)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := harness.client.Status().Update(harness.ctx, pod); err != nil {
		t.Fatalf("mark Pod not Ready: %v", err)
	}
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{}
	if err := harness.client.Get(harness.ctx, client.ObjectKey{Name: "endpoint-a"}, endpoint); err != nil {
		t.Fatalf("read Endpoint: %v", err)
	}
	endpoint.Spec.Unschedulable = true
	if err := harness.client.Update(harness.ctx, endpoint); err != nil {
		t.Fatalf("invalidate Endpoint: %v", err)
	}
	harness.reconcile(t)
	workload = harness.workload(t)
	if workload.Status.SelectedMode != acceleratorv1alpha1.ExecutionModeRemote ||
		workload.Status.AllocationRef != originalLease || workload.Status.Fallback.FailedAttempts != 0 ||
		len(workload.Status.Fallback.History) != 0 {
		t.Fatalf("post-Ready degradation incorrectly triggered fallback: %#v", workload.Status)
	}
	if workload.Status.Phase != acceleratorv1alpha1.WorkloadPhaseRunning {
		t.Fatalf("post-Ready degradation regressed the Workload to startup: %#v", workload.Status)
	}
	podReadyCondition := findMetaCondition(workload.Status.Conditions, ConditionPodReady)
	if podReadyCondition == nil || podReadyCondition.Status != metav1.ConditionFalse || podReadyCondition.Reason != "PreviouslyReady" {
		t.Fatalf("post-Ready degradation was not represented explicitly: %#v", workload.Status.Conditions)
	}
}

func TestFallbackStateSurvivesControllerReconstruction(t *testing.T) {
	harness := newControllerHarness(t, "restart-fallback", []acceleratorv1alpha1.ExecutionMode{
		acceleratorv1alpha1.ExecutionModeLocal,
		acceleratorv1alpha1.ExecutionModeRemote,
	}, nil)
	workload := harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeLocal)
	pod := harness.pod(t, workload)
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "selected node failed",
	}}
	if err := harness.client.Status().Update(harness.ctx, pod); err != nil {
		t.Fatalf("mark Pod unschedulable: %v", err)
	}
	harness.reconcile(t)
	workload = harness.workload(t)
	if len(workload.Status.Fallback.History) != 1 {
		t.Fatalf("failure history was not persisted before cleanup: %#v", workload.Status.Fallback)
	}

	harness.reconciler = harness.newReconciler()
	for step := 0; step < 10; step++ {
		harness.reconcile(t)
		workload = harness.workload(t)
		if workload.Status.AllocationRef == "" && workload.Status.Fallback.RequeueAt != nil {
			break
		}
	}
	if workload.Status.Fallback.RequeueAt == nil || !harness.now.Before(workload.Status.Fallback.RequeueAt.Time) {
		t.Fatalf("durable fallback deadline was not established: %#v", workload.Status.Fallback)
	}
	harness.reconciler = harness.newReconciler()
	if result, waiting := harness.reconciler.waitForFallback(workload); !waiting || result.RequeueAfter <= 0 {
		t.Fatalf("reconstructed controller ignored persisted backoff: result=%#v waiting=%t", result, waiting)
	}
	harness.now = workload.Status.Fallback.RequeueAt.Time.Add(time.Millisecond)
	workload = harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
	if len(workload.Status.Fallback.History) != 1 || workload.Status.Fallback.History[0].Targets[0].NodeName != "node-a" {
		t.Fatalf("reconstructed controller lost failed-target exclusion: %#v", workload.Status.Fallback)
	}
}

func TestWorkloadReconcilerRestoresAnExistingPodIdempotently(t *testing.T) {
	harness := newControllerHarness(t, "existing-pod", []acceleratorv1alpha1.ExecutionMode{
		acceleratorv1alpha1.ExecutionModeLocal,
		acceleratorv1alpha1.ExecutionModeRemote,
	}, nil)
	var workload *acceleratorv1alpha1.AcceleratorWorkload
	var lease *acceleratorv1alpha1.AcceleratorLease
	for step := 0; step < 10; step++ {
		harness.reconcile(t)
		workload = harness.workload(t)
		if workload.Status.AllocationRef == "" {
			continue
		}
		lease = &acceleratorv1alpha1.AcceleratorLease{}
		if err := harness.client.Get(harness.ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Status.AllocationRef}, lease); err != nil {
			t.Fatalf("read Lease: %v", err)
		}
		if lease.Status.Phase == acceleratorv1alpha1.AllocationPhaseReserved {
			break
		}
	}
	if lease == nil || lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseReserved {
		t.Fatalf("Lease did not reach Reserved: %#v", lease)
	}
	provider := &acceleratorv1alpha1.AcceleratorProvider{}
	pool := &acceleratorv1alpha1.AcceleratorPool{}
	if err := harness.client.Get(harness.ctx, client.ObjectKey{Name: lease.Spec.ProviderRef.Name}, provider); err != nil {
		t.Fatalf("read Provider: %v", err)
	}
	if err := harness.client.Get(harness.ctx, client.ObjectKey{Name: lease.Spec.PoolRef.Name}, pool); err != nil {
		t.Fatalf("read Pool: %v", err)
	}
	plan, err := localprovider.New().BuildRuntimePlan(harness.ctx, provider, pool, lease)
	if err != nil {
		t.Fatalf("build local plan: %v", err)
	}
	mutated, err := injector.NewPodMutator().Mutate(workload.Spec.PodTemplate, plan)
	if err != nil {
		t.Fatalf("mutate Pod template: %v", err)
	}
	pod := podFromTemplate(workload, lease, mutated)
	if err := controllerutil.SetControllerReference(workload, pod, harness.scheme); err != nil {
		t.Fatalf("set Pod owner: %v", err)
	}
	if err := harness.client.Create(harness.ctx, pod); err != nil {
		t.Fatalf("pre-create deterministic Pod: %v", err)
	}

	for step := 0; step < 3; step++ {
		harness.reconcile(t)
	}
	workload = harness.workload(t)
	if workload.Status.Phase != acceleratorv1alpha1.WorkloadPhaseStarting {
		t.Fatalf("existing Pod was not restored and bound: %#v", workload.Status)
	}
	pods := &corev1.PodList{}
	if err := harness.client.List(harness.ctx, pods, client.InNamespace(workload.Namespace)); err != nil {
		t.Fatalf("list Pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("repeated reconcile created duplicate Pods: %#v", pods.Items)
	}
}

func TestPrePodCapacityConflictFallsBackAcrossModes(t *testing.T) {
	conflict := &conflictingLocalAllocator{}
	harness := newControllerHarness(t, "reserve-fallback", []acceleratorv1alpha1.ExecutionMode{
		acceleratorv1alpha1.ExecutionModeLocal,
		acceleratorv1alpha1.ExecutionModeRemote,
	}, conflict)
	workload := harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
	if conflict.reserveCalls != 1 || conflict.releaseCalls == 0 {
		t.Fatalf("capacity conflict was not cleaned up exactly once: reserve=%d release=%d", conflict.reserveCalls, conflict.releaseCalls)
	}
	if len(workload.Status.Fallback.History) != 1 || workload.Status.Fallback.History[0].PodRef != nil ||
		workload.Status.Fallback.History[0].Reason != "CapacityChanged" {
		t.Fatalf("pre-Pod resource failure was not durably classified: %#v", workload.Status.Fallback)
	}
}

func TestPrePodRuntimePreparationConflictFallsBackAcrossModes(t *testing.T) {
	harness := newControllerHarness(t, "runtime-plan-fallback", []acceleratorv1alpha1.ExecutionMode{
		acceleratorv1alpha1.ExecutionModeLocal,
		acceleratorv1alpha1.ExecutionModeRemote,
	}, nil)
	localExecution, err := harness.adapters.Get(localprovider.DriverName)
	if err != nil {
		t.Fatalf("resolve local Adapter: %v", err)
	}
	remoteExecution, err := harness.adapters.Get(cricketprovider.DriverName)
	if err != nil {
		t.Fatalf("resolve remote Adapter: %v", err)
	}
	conflictProvider := &runtimeConflictProvider{Provider: localExecution.Provider()}
	localAllocator := &releaseRecordingAllocator{Allocator: localExecution.Allocator()}
	adapters, err := executionadapter.NewRegistry(
		executionadapter.NewBinding(conflictProvider, localAllocator),
		executionadapter.NewBinding(remoteExecution.Provider(), remoteExecution.Allocator()),
	)
	if err != nil {
		t.Fatalf("replace local Adapter: %v", err)
	}
	if remoteAllocator, ok := remoteExecution.Allocator().(*allocation.RemoteAllocator); ok {
		remoteAllocator.Validator = adapters.Providers()
	}
	harness.adapters = adapters
	harness.reconciler = harness.newReconciler()

	workload := harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
	if conflictProvider.buildCalls != 1 || localAllocator.releaseCalls == 0 {
		t.Fatalf("runtime preparation conflict was not released: builds=%d releases=%d", conflictProvider.buildCalls, localAllocator.releaseCalls)
	}
	if len(workload.Status.Fallback.History) != 1 || workload.Status.Fallback.History[0].PodRef != nil ||
		workload.Status.Fallback.History[0].Reason != "RuntimePreparationFailed" {
		t.Fatalf("pre-Pod runtime failure was not durably classified: %#v", workload.Status.Fallback)
	}
}

func TestTwoRemoteWorkloadsCannotAcquireSameEndpointWithStaleInventory(t *testing.T) {
	harness := newControllerHarness(t, "remote-owner-a", []acceleratorv1alpha1.ExecutionMode{
		acceleratorv1alpha1.ExecutionModeRemote,
		acceleratorv1alpha1.ExecutionModeLocal,
	}, nil)
	ownerA := harness.workload(t)
	ownerA.Spec.Placement = acceleratorv1alpha1.PlacementPolicy{
		Mode: acceleratorv1alpha1.PlacementModeRemoteOnly,
	}
	if err := harness.client.Update(harness.ctx, ownerA); err != nil {
		t.Fatalf("make first Workload RemoteOnly: %v", err)
	}
	ownerB := ownerA.DeepCopy()
	ownerB.ObjectMeta = metav1.ObjectMeta{
		Name: "remote-owner-b", Namespace: ownerA.Namespace,
		UID: types.UID("remote-owner-b-uid"), Generation: 1,
	}
	ownerB.Status = acceleratorv1alpha1.AcceleratorWorkloadStatus{}
	if err := harness.client.Create(harness.ctx, ownerB); err != nil {
		t.Fatalf("create competing Workload: %v", err)
	}

	// Deliberately ignore active Leases in discovery so both Workloads select
	// the same Endpoint. The allocator's claim remains the final ownership
	// check even when inventory is stale.
	harness.reconciler.Inventory = staticInventoryReader{candidates: []contracts.Candidate{{
		ID: "remote/remote/endpoint-a", ProviderName: "remote", PoolName: "remote",
		Mode: acceleratorv1alpha1.ExecutionModeRemote, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
		EndpointName: "endpoint-a", Available: 1, Exclusive: true, Ready: true,
		Devices: []contracts.Device{{
			ID: "gpu-0", Vendor: "nvidia", Model: "A100", Memory: resource.MustParse("40Gi"),
			ResourceForm: acceleratorv1alpha1.ResourceFormWholeDevice,
		}},
	}}}

	// Add each finalizer and create both allocation Leases before either
	// reservation reaches the atomic Endpoint claim.
	for step := 0; step < 2; step++ {
		harness.reconcile(t)
	}
	ownerA = harness.workload(t)
	if ownerA.Status.AllocationRef == "" {
		t.Fatalf("first Workload did not select the Endpoint: %#v", ownerA.Status)
	}
	requestB := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ownerB.Namespace, Name: ownerB.Name}}
	for step := 0; step < 2; step++ {
		if _, err := harness.reconciler.Reconcile(harness.ctx, requestB); err != nil {
			t.Fatalf("select Endpoint for competing Workload step %d: %v", step, err)
		}
	}
	if err := harness.client.Get(harness.ctx, requestB.NamespacedName, ownerB); err != nil {
		t.Fatalf("read competing Workload selection: %v", err)
	}
	if ownerB.Status.AllocationRef == "" {
		t.Fatalf("stale inventory did not produce a competing selection: %#v", ownerB.Status)
	}
	losingLeaseName := ownerB.Status.AllocationRef

	ownerA = harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
	for step := 0; step < 2; step++ {
		if _, err := harness.reconciler.Reconcile(harness.ctx, requestB); err != nil {
			t.Fatalf("reserve Endpoint for competing Workload step %d: %v", step, err)
		}
	}
	if err := harness.client.Get(harness.ctx, requestB.NamespacedName, ownerB); err != nil {
		t.Fatalf("read competing Workload after reservation: %v", err)
	}
	reserved := findMetaCondition(ownerB.Status.Conditions, ConditionReserved)
	if ownerA.Status.Phase != acceleratorv1alpha1.WorkloadPhaseStarting ||
		ownerB.Status.AllocationRef != "" || ownerB.Status.PodRef != nil ||
		reserved == nil || reserved.Status != metav1.ConditionFalse || reserved.Reason != "CapacityChanged" {
		t.Fatalf("competing Workload acquired or retained the Endpoint: ownerA=%#v ownerB=%#v", ownerA.Status, ownerB.Status)
	}
	if holder := harness.claimer.holders["endpoint-a"]; holder == "" {
		t.Fatal("winning Workload did not retain the Endpoint claim")
	}

	// The losing Failed Lease never owned the Endpoint and has already had its
	// release finalizer removed. Deleting that Workload must not try to release
	// the winner's claim or wedge its own finalizer.
	if err := harness.client.Delete(harness.ctx, ownerB); err != nil {
		t.Fatalf("delete losing Workload: %v", err)
	}
	for step := 0; step < 8; step++ {
		if _, err := harness.reconciler.Reconcile(harness.ctx, requestB); err != nil {
			t.Fatalf("finalize losing Workload step %d: %v", step, err)
		}
		if err := harness.client.Get(harness.ctx, requestB.NamespacedName, &acceleratorv1alpha1.AcceleratorWorkload{}); apierrors.IsNotFound(err) {
			break
		}
	}
	if err := harness.client.Get(harness.ctx, requestB.NamespacedName, &acceleratorv1alpha1.AcceleratorWorkload{}); !apierrors.IsNotFound(err) {
		t.Fatalf("losing Workload remained after finalization: %v", err)
	}
	if holder := harness.claimer.holders["endpoint-a"]; holder == "" {
		t.Fatal("losing Workload deletion removed the winner's Endpoint claim")
	}
	losingLease := &acceleratorv1alpha1.AcceleratorLease{}
	if err := harness.client.Get(harness.ctx, client.ObjectKey{Namespace: ownerB.Namespace, Name: losingLeaseName}, losingLease); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("read losing Lease after Workload deletion: %v", err)
	}
}

func TestWorkloadAndLeaseDeletionReleaseRemoteCapacity(t *testing.T) {
	t.Run("Workload deletion", func(t *testing.T) {
		harness := newControllerHarness(t, "delete-workload", []acceleratorv1alpha1.ExecutionMode{
			acceleratorv1alpha1.ExecutionModeRemote,
			acceleratorv1alpha1.ExecutionModeLocal,
		}, nil)
		workload := harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
		if err := harness.client.Delete(harness.ctx, workload); err != nil {
			t.Fatalf("delete Workload: %v", err)
		}
		for step := 0; step < 12; step++ {
			_, err := harness.reconciler.Reconcile(harness.ctx, harness.request)
			if err != nil {
				t.Fatalf("finalize Workload step %d: %v", step, err)
			}
			current := &acceleratorv1alpha1.AcceleratorWorkload{}
			if err := harness.client.Get(harness.ctx, harness.request.NamespacedName, current); apierrors.IsNotFound(err) {
				break
			}
		}
		if holder := harness.claimer.holders["endpoint-a"]; holder != "" {
			t.Fatalf("Workload deletion leaked endpoint holder %q", holder)
		}
	})

	t.Run("configuration deletion before reservation", func(t *testing.T) {
		harness := newControllerHarness(t, "delete-config-before-reserve", []acceleratorv1alpha1.ExecutionMode{
			acceleratorv1alpha1.ExecutionModeRemote,
			acceleratorv1alpha1.ExecutionModeLocal,
		}, nil)
		workload := harness.workload(t)
		workload.Spec.Placement = acceleratorv1alpha1.PlacementPolicy{Mode: acceleratorv1alpha1.PlacementModeRemoteOnly}
		if err := harness.client.Update(harness.ctx, workload); err != nil {
			t.Fatalf("make Workload RemoteOnly: %v", err)
		}

		var lease *acceleratorv1alpha1.AcceleratorLease
		for step := 0; step < 8; step++ {
			harness.reconcile(t)
			workload = harness.workload(t)
			if workload.Status.AllocationRef == "" {
				continue
			}
			lease = &acceleratorv1alpha1.AcceleratorLease{}
			if err := harness.client.Get(harness.ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Status.AllocationRef}, lease); err != nil {
				t.Fatalf("read pending Lease: %v", err)
			}
			if lease.Status.Phase == acceleratorv1alpha1.AllocationPhasePending {
				break
			}
		}
		if lease == nil || lease.Status.Phase != acceleratorv1alpha1.AllocationPhasePending || lease.Spec.Driver != cricketprovider.DriverName {
			t.Fatalf("Lease did not reach Pending with a driver snapshot: %#v", lease)
		}
		if err := harness.client.Delete(harness.ctx, &acceleratorv1alpha1.AcceleratorPool{ObjectMeta: metav1.ObjectMeta{Name: "remote"}}); err != nil {
			t.Fatalf("delete remote Pool: %v", err)
		}
		if err := harness.client.Delete(harness.ctx, &acceleratorv1alpha1.AcceleratorProvider{ObjectMeta: metav1.ObjectMeta{Name: "remote"}}); err != nil {
			t.Fatalf("delete remote Provider: %v", err)
		}
		harness.reconcile(t)
		workload = harness.workload(t)
		if workload.Status.Phase != acceleratorv1alpha1.WorkloadPhasePending || workload.Status.AllocationRef != "" {
			t.Fatalf("pre-reservation configuration deletion did not return to selection: %#v", workload.Status)
		}
		failedLease := &acceleratorv1alpha1.AcceleratorLease{}
		if err := harness.client.Get(harness.ctx, client.ObjectKeyFromObject(lease), failedLease); err != nil {
			t.Fatalf("read failed Lease: %v", err)
		}
		if failedLease.Status.Phase != acceleratorv1alpha1.AllocationPhaseFailed || controllerutil.ContainsFinalizer(failedLease, LeaseFinalizer) {
			t.Fatalf("pre-reservation Lease was not terminalized cleanly: %#v", failedLease)
		}
		if holder := harness.claimer.holders["endpoint-a"]; holder != "" {
			t.Fatalf("pre-reservation failure unexpectedly acquired Endpoint holder %q", holder)
		}
	})

	t.Run("configuration deletion before Pod creation", func(t *testing.T) {
		harness := newControllerHarness(t, "delete-config-before-pod", []acceleratorv1alpha1.ExecutionMode{
			acceleratorv1alpha1.ExecutionModeRemote,
			acceleratorv1alpha1.ExecutionModeLocal,
		}, nil)
		workload := harness.workload(t)
		workload.Spec.Placement = acceleratorv1alpha1.PlacementPolicy{Mode: acceleratorv1alpha1.PlacementModeRemoteOnly}
		if err := harness.client.Update(harness.ctx, workload); err != nil {
			t.Fatalf("make Workload RemoteOnly: %v", err)
		}

		var lease *acceleratorv1alpha1.AcceleratorLease
		for step := 0; step < 8; step++ {
			harness.reconcile(t)
			workload = harness.workload(t)
			if workload.Status.AllocationRef == "" {
				continue
			}
			lease = &acceleratorv1alpha1.AcceleratorLease{}
			if err := harness.client.Get(harness.ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Status.AllocationRef}, lease); err != nil {
				t.Fatalf("read reserved Lease: %v", err)
			}
			if lease.Status.Phase == acceleratorv1alpha1.AllocationPhaseReserved {
				break
			}
		}
		if lease == nil || lease.Status.Phase != acceleratorv1alpha1.AllocationPhaseReserved || lease.Spec.Driver != cricketprovider.DriverName {
			t.Fatalf("Lease did not persist its selected driver before configuration deletion: %#v", lease)
		}
		if err := harness.client.Delete(harness.ctx, &acceleratorv1alpha1.AcceleratorPool{ObjectMeta: metav1.ObjectMeta{Name: "remote"}}); err != nil {
			t.Fatalf("delete remote Pool: %v", err)
		}
		if err := harness.client.Delete(harness.ctx, &acceleratorv1alpha1.AcceleratorProvider{ObjectMeta: metav1.ObjectMeta{Name: "remote"}}); err != nil {
			t.Fatalf("delete remote Provider: %v", err)
		}
		harness.reconcile(t)
		workload = harness.workload(t)
		if workload.Status.Phase != acceleratorv1alpha1.WorkloadPhaseFailed {
			t.Fatalf("configuration deletion did not fail cleanly: %#v", workload.Status)
		}
		if holder := harness.claimer.holders["endpoint-a"]; holder != "" {
			t.Fatalf("configuration deletion leaked Endpoint claim %q", holder)
		}
	})

	t.Run("Workload deletion after configuration deletion", func(t *testing.T) {
		harness := newControllerHarness(t, "delete-workload-and-config", []acceleratorv1alpha1.ExecutionMode{
			acceleratorv1alpha1.ExecutionModeRemote,
			acceleratorv1alpha1.ExecutionModeLocal,
		}, nil)
		workload := harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
		if err := harness.client.Delete(harness.ctx, &acceleratorv1alpha1.AcceleratorPool{ObjectMeta: metav1.ObjectMeta{Name: "remote"}}); err != nil {
			t.Fatalf("delete remote Pool: %v", err)
		}
		if err := harness.client.Delete(harness.ctx, &acceleratorv1alpha1.AcceleratorProvider{ObjectMeta: metav1.ObjectMeta{Name: "remote"}}); err != nil {
			t.Fatalf("delete remote Provider: %v", err)
		}
		if err := harness.client.Delete(harness.ctx, workload); err != nil {
			t.Fatalf("delete Workload: %v", err)
		}
		for step := 0; step < 12; step++ {
			if _, err := harness.reconciler.Reconcile(harness.ctx, harness.request); err != nil {
				t.Fatalf("finalize Workload without configuration step %d: %v", step, err)
			}
			if err := harness.client.Get(harness.ctx, harness.request.NamespacedName, &acceleratorv1alpha1.AcceleratorWorkload{}); apierrors.IsNotFound(err) {
				break
			}
		}
		if holder := harness.claimer.holders["endpoint-a"]; holder != "" {
			t.Fatalf("Workload deletion without configuration leaked Endpoint holder %q", holder)
		}
	})

	t.Run("Lease deletion", func(t *testing.T) {
		harness := newControllerHarness(t, "delete-lease", []acceleratorv1alpha1.ExecutionMode{
			acceleratorv1alpha1.ExecutionModeRemote,
			acceleratorv1alpha1.ExecutionModeLocal,
		}, nil)
		workload := harness.runUntilStarting(t, acceleratorv1alpha1.ExecutionModeRemote)
		lease := &acceleratorv1alpha1.AcceleratorLease{}
		leaseKey := client.ObjectKey{Namespace: workload.Namespace, Name: workload.Status.AllocationRef}
		if err := harness.client.Get(harness.ctx, leaseKey, lease); err != nil {
			t.Fatalf("read Lease: %v", err)
		}
		if err := harness.client.Delete(harness.ctx, lease); err != nil {
			t.Fatalf("delete Lease: %v", err)
		}
		for step := 0; step < 12; step++ {
			harness.reconcile(t)
			if err := harness.client.Get(harness.ctx, leaseKey, &acceleratorv1alpha1.AcceleratorLease{}); apierrors.IsNotFound(err) {
				break
			}
		}
		if holder := harness.claimer.holders["endpoint-a"]; holder != "" {
			t.Fatalf("Lease deletion leaked endpoint holder %q", holder)
		}
		workload = harness.workload(t)
		if workload.Status.Phase != acceleratorv1alpha1.WorkloadPhaseFailed {
			t.Fatalf("direct Lease deletion did not terminalize Workload: %#v", workload.Status)
		}
	})
}

func TestControllerSelectsConfiguredMIGAndAnotherLocalAdapter(t *testing.T) {
	tests := []struct {
		name         string
		driver       string
		resourceName corev1.ResourceName
		resourceForm acceleratorv1alpha1.ResourceForm
		profile      string
		count        int32
	}{
		{
			name: "MIG", driver: localprovider.DriverName,
			resourceName: "nvidia.com/mig-1g.10gb", resourceForm: acceleratorv1alpha1.ResourceFormMIG,
			profile: "1g.10gb", count: 2,
		},
		{
			name: "another Device Plugin driver", driver: "example-share-device-plugin",
			resourceName: "example.com/gpu-share", resourceForm: acceleratorv1alpha1.ResourceFormShared,
			profile: "5Gi-share", count: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runLocalAdapterScenario(t, test.driver, test.resourceName, test.resourceForm, test.profile, test.count)
		})
	}
}

func runLocalAdapterScenario(
	t *testing.T,
	driver string,
	resourceName corev1.ResourceName,
	resourceForm acceleratorv1alpha1.ResourceForm,
	profile string,
	count int32,
) {
	t.Helper()
	ctx := context.Background()
	scheme := controllerTestScheme(t)
	provider := &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-local", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver: driver, DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
		},
	}
	pool := &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-local", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: provider.Name},
			Location:    acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Local: &acceleratorv1alpha1.LocalPoolConfig{
				ResourceName: resourceName, ResourceForm: resourceForm, Profile: profile,
			},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubernetes.io/hostname": "node-a"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{resourceName: resource.MustParse("8")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	workload := &acceleratorv1alpha1.AcceleratorWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-adapter", Namespace: "default", UID: types.UID("custom-adapter-uid"), Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorWorkloadSpec{
			Accelerator: acceleratorv1alpha1.AcceleratorRequest{
				DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: count,
				ResourceForm: resourceForm, Profile: profile,
			},
			Placement: acceleratorv1alpha1.PlacementPolicy{Mode: acceleratorv1alpha1.PlacementModeLocalOnly},
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: "example.test/workload"}},
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&acceleratorv1alpha1.AcceleratorWorkload{}, &acceleratorv1alpha1.AcceleratorLease{}, &corev1.Pod{}).
		WithObjects(provider, pool, node, workload).Build()
	adapters, err := executionadapter.NewRegistry(executionadapter.NewBinding(
		localprovider.NewWithDriver(driver), &allocation.LocalAllocator{},
	))
	if err != nil {
		t.Fatalf("create custom Adapter: %v", err)
	}
	reconciler := &WorkloadReconciler{
		Client: kubeClient, Scheme: scheme,
		Inventory: inventory.NewKubernetesReader(kubeClient, adapters.Providers()),
		Policy:    policy.NewLocalFirst(), Adapters: adapters, Injector: injector.NewPodMutator(),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workload.Namespace, Name: workload.Name}}
	for step := 0; step < 10; step++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("reconcile custom Adapter step %d: %v", step, err)
		}
		if err := kubeClient.Get(ctx, request.NamespacedName, workload); err != nil {
			t.Fatalf("read custom Workload: %v", err)
		}
		if workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseStarting {
			break
		}
	}
	pod := &corev1.Pod{}
	if workload.Status.PodRef == nil {
		t.Fatalf("custom Adapter did not create a Pod: %#v", workload.Status)
	}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Status.PodRef.Name}, pod); err != nil {
		t.Fatalf("read custom Adapter Pod: %v", err)
	}
	if got := pod.Spec.Containers[0].Resources.Limits[resourceName]; got.Value() != int64(count) {
		t.Fatalf("custom Adapter injected %s, want %d", got.String(), count)
	}
}

func TestRuntimePlanModeBoundaryRejectsMixedResources(t *testing.T) {
	resources := injector.ResourceRequirements("nvidia.com/gpu", 1)
	lease := &acceleratorv1alpha1.AcceleratorLease{Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
		Mode: acceleratorv1alpha1.ExecutionModeRemote,
		Targets: []acceleratorv1alpha1.AllocationTarget{{
			EndpointName: "endpoint-a", DeviceIDs: []string{"gpu-0"}, Exclusive: true,
		}},
	}}
	err := validateRuntimePlan(contracts.RuntimePlan{
		Mode: acceleratorv1alpha1.ExecutionModeRemote, Resources: &resources,
	}, lease, map[corev1.ResourceName]struct{}{"nvidia.com/gpu": {}})
	if !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("Remote plan with local resource should be invalid, got %v", err)
	}
	if err := validateRuntimePlan(contracts.RuntimePlan{Mode: acceleratorv1alpha1.ExecutionModeLocal}, lease, nil); !contracts.IsErrorKind(err, contracts.ErrorKindInvalid) {
		t.Fatalf("plan/Lease mode mismatch should be invalid, got %v", err)
	}
}

type controllerHarness struct {
	ctx        context.Context
	scheme     *runtime.Scheme
	client     client.Client
	adapters   *executionadapter.Registry
	claimer    *controllerEndpointClaimer
	reconciler *WorkloadReconciler
	request    ctrl.Request
	now        time.Time
}

func newControllerHarness(
	t *testing.T,
	name string,
	order []acceleratorv1alpha1.ExecutionMode,
	localAllocator contracts.Allocator,
) *controllerHarness {
	t.Helper()
	ctx := context.Background()
	scheme := controllerTestScheme(t)
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
				Image: "example.test/cricket-client", RuntimeVersion: "test-version",
			},
		},
	}
	localPool := &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: "local"},
			Location:    acceleratorv1alpha1.ExecutionModeLocal, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Priority: 100, Local: &acceleratorv1alpha1.LocalPoolConfig{
				ResourceName: "nvidia.com/gpu", ResourceForm: acceleratorv1alpha1.ResourceFormWholeDevice,
			},
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
		Status:     observedEndpointStatusForTest(1),
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint-a", Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: "remote"}, Address: "gpu.example",
			Runtime:    acceleratorv1alpha1.RuntimeInfo{Driver: cricketprovider.DriverName, Version: "test-version", APIVersion: "1"},
			Parameters: map[string]string{cricketprovider.ParameterRPCID: "1"},
			Devices: []acceleratorv1alpha1.DeviceInfo{{
				ID: "gpu-0", Vendor: "nvidia", Model: "A100", Memory: resource.MustParse("40Gi"),
				ResourceForm: acceleratorv1alpha1.ResourceFormWholeDevice,
				Health:       acceleratorv1alpha1.DeviceHealthHealthy,
			}},
		},
	}
	workload := &acceleratorv1alpha1.AcceleratorWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid"), Generation: 1},
		Spec: acceleratorv1alpha1.AcceleratorWorkloadSpec{
			Accelerator: acceleratorv1alpha1.AcceleratorRequest{DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: 1},
			Placement: acceleratorv1alpha1.PlacementPolicy{
				Mode: acceleratorv1alpha1.PlacementModeAuto, Order: order,
				Fallback: &acceleratorv1alpha1.FallbackPolicy{
					Enabled: true, MaxAttempts: 2,
					StartupTimeout: metav1.Duration{Duration: time.Minute}, Backoff: metav1.Duration{Duration: time.Second},
				},
			},
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "worker", Image: "example.test/workload"}},
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
	harness := &controllerHarness{
		ctx: ctx, scheme: scheme, client: kubeClient,
		claimer: &controllerEndpointClaimer{},
		request: ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workload.Namespace, Name: workload.Name}},
		now:     time.Unix(1000, 0),
	}
	if localAllocator == nil {
		localAllocator = &allocation.LocalAllocator{Now: func() time.Time { return harness.now }}
	}
	remoteAllocator := &allocation.RemoteAllocator{
		Resolver: &allocation.KubernetesEndpointResolver{Client: kubeClient},
		Claimer:  harness.claimer, Now: func() time.Time { return harness.now },
	}
	adapters, err := executionadapter.NewRegistry(
		executionadapter.NewBinding(localprovider.New(), localAllocator),
		executionadapter.NewBinding(newObservedCricketForTest(), remoteAllocator),
	)
	if err != nil {
		t.Fatalf("create Adapter registry: %v", err)
	}
	remoteAllocator.Validator = adapters.Providers()
	harness.adapters = adapters
	harness.reconciler = harness.newReconciler()
	return harness
}

func controllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kubernetes scheme: %v", err)
	}
	if err := acceleratorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add accelerator scheme: %v", err)
	}
	return scheme
}

func (h *controllerHarness) newReconciler() *WorkloadReconciler {
	return &WorkloadReconciler{
		Client: h.client, Scheme: h.scheme,
		Inventory: inventory.NewKubernetesReader(h.client, h.adapters.Providers()),
		Policy:    policy.NewLocalFirst(), Adapters: h.adapters, Injector: injector.NewPodMutator(),
		Now: func() time.Time { return h.now },
	}
}

func (h *controllerHarness) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	result, err := h.reconciler.Reconcile(h.ctx, h.request)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return result
}

func (h *controllerHarness) workload(t *testing.T) *acceleratorv1alpha1.AcceleratorWorkload {
	t.Helper()
	workload := &acceleratorv1alpha1.AcceleratorWorkload{}
	if err := h.client.Get(h.ctx, h.request.NamespacedName, workload); err != nil {
		t.Fatalf("read Workload: %v", err)
	}
	return workload
}

func (h *controllerHarness) pod(t *testing.T, workload *acceleratorv1alpha1.AcceleratorWorkload) *corev1.Pod {
	t.Helper()
	if workload.Status.PodRef == nil {
		t.Fatalf("Workload has no Pod reference: %#v", workload.Status)
	}
	pod := &corev1.Pod{}
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Status.PodRef.Name}, pod); err != nil {
		t.Fatalf("read Pod: %v", err)
	}
	return pod
}

func (h *controllerHarness) runUntilStarting(t *testing.T, mode acceleratorv1alpha1.ExecutionMode) *acceleratorv1alpha1.AcceleratorWorkload {
	t.Helper()
	for step := 0; step < 30; step++ {
		h.reconcile(t)
		workload := h.workload(t)
		if workload.Status.Fallback.RequeueAt != nil && h.now.Before(workload.Status.Fallback.RequeueAt.Time) && workload.Status.AllocationRef == "" {
			h.now = workload.Status.Fallback.RequeueAt.Time.Add(time.Millisecond)
		}
		if workload.Status.SelectedMode == mode && workload.Status.Phase == acceleratorv1alpha1.WorkloadPhaseStarting {
			return workload
		}
	}
	t.Fatalf("Workload did not reach Starting in %s mode: %#v", mode, h.workload(t).Status)
	return nil
}

type conflictingLocalAllocator struct {
	reserveCalls int
	releaseCalls int
}

func (a *conflictingLocalAllocator) Reserve(context.Context, contracts.ReservationRequest) (contracts.Reservation, error) {
	a.reserveCalls++
	return contracts.Reservation{}, contracts.NewError(contracts.ErrorKindConflict, "reserve test local capacity", "capacity changed")
}

func (a *conflictingLocalAllocator) Bind(context.Context, contracts.ReservationRequest, corev1.ObjectReference) error {
	return nil
}

func (a *conflictingLocalAllocator) Release(context.Context, contracts.ReservationRequest) error {
	a.releaseCalls++
	return nil
}

type runtimeConflictProvider struct {
	contracts.Provider
	buildCalls int
}

func (p *runtimeConflictProvider) BuildRuntimePlan(
	context.Context,
	*acceleratorv1alpha1.AcceleratorProvider,
	*acceleratorv1alpha1.AcceleratorPool,
	*acceleratorv1alpha1.AcceleratorLease,
) (contracts.RuntimePlan, error) {
	p.buildCalls++
	return contracts.RuntimePlan{}, contracts.NewError(contracts.ErrorKindConflict, "prepare test runtime", "runtime capacity changed")
}

type releaseRecordingAllocator struct {
	contracts.Allocator
	releaseCalls int
}

func (a *releaseRecordingAllocator) Release(ctx context.Context, request contracts.ReservationRequest) error {
	a.releaseCalls++
	return a.Allocator.Release(ctx, request)
}

type staticInventoryReader struct {
	candidates []contracts.Candidate
}

func (r staticInventoryReader) ListCandidates(context.Context, contracts.SelectionRequest) ([]contracts.Candidate, error) {
	return append([]contracts.Candidate(nil), r.candidates...), nil
}
