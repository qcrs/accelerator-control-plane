package controller_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goRuntime "runtime"
	"testing"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// This test starts a real kube-apiserver and etcd, installs the checked-in
// CRDs, verifies schema defaulting, and exercises the remote endpoint CAS.
// It is skipped by normal unit-test runs unless setup-envtest assets are set.
func TestCRDsAndAtomicEndpointClaimsWithEnvtest(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run with setup-envtest assets")
	}
	crdDirectory := os.Getenv("ACCELERATOR_CRD_DIR")
	if crdDirectory == "" {
		crdDirectory = filepath.Join("..", "..", "config", "crd", "bases")
	}
	testEnvironment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{crdDirectory},
		ErrorIfCRDPathMissing: true,
	}
	configuration, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			if goRuntime.GOOS == "windows" {
				if cleanupErr := stopWindowsEnvtestProcesses(assets); cleanupErr != nil {
					t.Errorf("stop envtest: %v; Windows cleanup also failed: %v", err, cleanupErr)
				} else {
					t.Logf("controller-runtime could not signal envtest on Windows; force-stopped test processes: %v", err)
				}
				return
			}
			t.Errorf("stop envtest: %v", err)
		}
	})

	scheme := k8sruntime.NewScheme()
	for name, add := range map[string]func(*k8sruntime.Scheme) error{
		"accelerator":  acceleratorv1alpha1.AddToScheme,
		"coordination": coordinationv1.AddToScheme,
		"core":         corev1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add %s scheme: %v", name, err)
		}
	}
	kubeClient, err := client.New(configuration, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create envtest client: %v", err)
	}
	ctx := context.Background()
	for _, namespace := range []string{"accelerator-system"} {
		if err := kubeClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Fatalf("create namespace %s: %v", namespace, err)
		}
	}

	provider := &acceleratorv1alpha1.AcceleratorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "remote"},
		Spec: acceleratorv1alpha1.AcceleratorProviderSpec{
			Driver: "cricket", DeviceKinds: []acceleratorv1alpha1.DeviceKind{acceleratorv1alpha1.DeviceKindGPU},
			ClientRuntime: &acceleratorv1alpha1.ClientRuntimeSpec{Image: "example.test/cricket:v1", RuntimeVersion: "v1"},
		},
	}
	if err := kubeClient.Create(ctx, provider); err != nil {
		t.Fatalf("create Provider through CRD: %v", err)
	}
	pool := &acceleratorv1alpha1.AcceleratorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "remote"},
		Spec: acceleratorv1alpha1.AcceleratorPoolSpec{
			ProviderRef: acceleratorv1alpha1.ClusterObjectReference{Name: provider.Name},
			Location:    acceleratorv1alpha1.ExecutionModeRemote, DeviceType: acceleratorv1alpha1.DeviceKindGPU,
			Remote: &acceleratorv1alpha1.RemotePoolConfig{},
		},
	}
	if err := kubeClient.Create(ctx, pool); err != nil {
		t.Fatalf("create Pool through CRD: %v", err)
	}
	persistedPool := &acceleratorv1alpha1.AcceleratorPool{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: pool.Name}, persistedPool); err != nil {
		t.Fatalf("read defaulted Pool: %v", err)
	}
	if persistedPool.Spec.Remote.AllocationMode != acceleratorv1alpha1.RemoteAllocationModeExclusiveEndpoint {
		t.Fatalf("CRD did not default remote allocation mode: %#v", persistedPool.Spec.Remote)
	}
	endpoint := &acceleratorv1alpha1.AcceleratorEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint-a"},
		Spec: acceleratorv1alpha1.AcceleratorEndpointSpec{
			PoolRef: acceleratorv1alpha1.ClusterObjectReference{Name: pool.Name}, Address: "gpu-a.example",
			Runtime: acceleratorv1alpha1.RuntimeInfo{Driver: "cricket", Version: "v1", APIVersion: "1"},
			Devices: []acceleratorv1alpha1.DeviceInfo{{
				ID: "gpu-0", UUID: "GPU-0000", Health: acceleratorv1alpha1.DeviceHealthHealthy,
				Memory: resource.MustParse("40Gi"),
			}},
		},
	}
	if err := kubeClient.Create(ctx, endpoint); err != nil {
		t.Fatalf("create Endpoint through CRD: %v", err)
	}

	ownerA := createEnvtestAllocationLease(t, ctx, kubeClient, "owner-a", endpoint.Name)
	ownerB := createEnvtestAllocationLease(t, ctx, kubeClient, "owner-b", endpoint.Name)
	claimer := &allocation.KubernetesEndpointClaimer{
		Client: kubeClient, APIReader: kubeClient, Namespace: "accelerator-system", Now: func() time.Time { return time.Unix(100, 0) },
	}
	claimA := allocation.EndpointClaim{
		EndpointName: endpoint.Name, HolderKey: string(ownerA.UID), LeaseNamespace: ownerA.Namespace, LeaseName: ownerA.Name, TTL: time.Minute,
	}
	if err := claimer.Claim(ctx, claimA); err != nil {
		t.Fatalf("claim endpoint for first owner: %v", err)
	}
	if err := claimer.Claim(ctx, claimA); err != nil {
		t.Fatalf("repeat claim must be idempotent: %v", err)
	}
	claimB := allocation.EndpointClaim{
		EndpointName: endpoint.Name, HolderKey: string(ownerB.UID), LeaseNamespace: ownerB.Namespace, LeaseName: ownerB.Name, TTL: time.Minute,
	}
	if err := claimer.Claim(ctx, claimB); !contracts.IsErrorKind(err, contracts.ErrorKindConflict) {
		t.Fatalf("second owner should observe an atomic claim conflict, got %v", err)
	}
	if err := claimer.Release(ctx, claimA); err != nil {
		t.Fatalf("release first endpoint claim: %v", err)
	}
	if err := claimer.Claim(ctx, claimB); err != nil {
		t.Fatalf("second owner could not claim after release: %v", err)
	}
}

func stopWindowsEnvtestProcesses(assets string) error {
	const script = `$root=$env:ACCELERATOR_ENVTEST_ASSETS; Get-Process | Where-Object { $_.ProcessName -in @('kube-apiserver','etcd') -and $_.Path -like "$root*" } | Stop-Process -Force`
	command := exec.Command("powershell", "-NoProfile", "-Command", script)
	command.Env = append(os.Environ(), "ACCELERATOR_ENVTEST_ASSETS="+assets)
	return command.Run()
}

func createEnvtestAllocationLease(
	t *testing.T,
	ctx context.Context,
	kubeClient client.Client,
	name, endpointName string,
) *acceleratorv1alpha1.AcceleratorLease {
	t.Helper()
	lease := &acceleratorv1alpha1.AcceleratorLease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: acceleratorv1alpha1.AcceleratorLeaseSpec{
			Owner: corev1.ObjectReference{
				APIVersion: acceleratorv1alpha1.GroupVersion.String(), Kind: "AcceleratorWorkload", Namespace: "default", Name: name,
			},
			WorkloadGeneration: 1,
			Request:            acceleratorv1alpha1.AcceleratorRequest{DeviceType: acceleratorv1alpha1.DeviceKindGPU, Count: 1},
			ProviderRef:        acceleratorv1alpha1.ClusterObjectReference{Name: "remote"},
			PoolRef:            acceleratorv1alpha1.ClusterObjectReference{Name: "remote"},
			Mode:               acceleratorv1alpha1.ExecutionModeRemote,
			Targets: []acceleratorv1alpha1.AllocationTarget{{
				EndpointName: endpointName, DeviceIDs: []string{"gpu-0"}, Exclusive: true,
			}},
			TTL: metav1.Duration{Duration: time.Minute},
		},
	}
	if err := kubeClient.Create(ctx, lease); err != nil {
		t.Fatalf("create allocation Lease %s through CRD: %v", name, err)
	}
	return lease
}
