#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
evidence_root="$repo_root/docs/evidence/platform-adapter"
log_dir="$evidence_root/logs"
mkdir -p "$log_dir"
cd "$repo_root"

{
    echo "evidence timestamp: $(date -Is)"
    echo "repository: $repo_root"
    go version
    echo
    echo "[adapter registry]"
    go test ./internal/adapter -count=1 -v
    echo
    echo "[inventory and capacity]"
    go test ./internal/inventory -count=1 -v
    echo
    echo "[pod mutation algorithm]"
    go test ./internal/injector -count=1 -v
    echo
    echo "[local allocator and state machine]"
    go test ./internal/allocation \
        -run 'TestLocalAllocator|TestAllocationTransitions|TestIsExpired' \
        -count=1 -v
    echo
    echo "[local provider runtime plan]"
    go test ./internal/provider/local -count=1 -v
} >"$log_dir/P-01-core-unit-tests.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "test scope: LocalOnly / RemoteOnly(mock) / Auto / filtering / stable selection"
    go test ./internal/policy -run 'TestLocalFirst' -count=1 -v
} >"$log_dir/P-02-policy-matrix.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "test scope: fake-client lifecycle, idempotent restore, controller reconstruction"
    go test ./internal/controller \
        -run 'TestWorkloadReconcilerLocalLifecycle|TestWorkloadReconcilerRestoresAnExistingPodIdempotently|TestFallbackStateSurvivesControllerReconstruction' \
        -count=1 -v
} >"$log_dir/P-03-controller-lifecycle.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "test scope: capacity conflict, mode boundary, endpoint claim, release"
    go test ./internal/controller \
        -run 'TestPrePodCapacityConflictFallsBackAcrossModes|TestRuntimePlanModeBoundaryRejectsMixedResources|TestTwoRemoteWorkloadsCannotAcquireSameEndpointWithStaleInventory|TestWorkloadAndLeaseDeletionReleaseRemoteCapacity' \
        -count=1 -v
} >"$log_dir/P-04-conflict-boundary-release.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    export KUBEBUILDER_ASSETS="$(setup-envtest use 1.36.2 -p path)"
    echo "KUBEBUILDER_ASSETS=$KUBEBUILDER_ASSETS"
    go test ./internal/controller \
        -run TestCRDsAndAtomicEndpointClaimsWithEnvtest \
        -count=1 -v
} >"$log_dir/P-05-envtest.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "kubectl context: $(kubectl config current-context)"
    echo "API readiness: $(kubectl get --raw=/readyz)"
    echo
    echo "[installed CRDs]"
    kubectl get crd \
        acceleratorproviders.accelerator.qcrs.io \
        acceleratorpools.accelerator.qcrs.io \
        acceleratorworkloads.accelerator.qcrs.io \
        acceleratorleases.accelerator.qcrs.io \
        acceleratorendpoints.accelerator.qcrs.io
    echo
    echo "[server-side dry-run]"
    kubectl apply --server-side --dry-run=server -f "$evidence_root/mock-dry-run.yaml"
    echo
    echo "No object was persisted by this command."
} >"$log_dir/P-06-crd-dry-run.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "control-chain evidence matrix (all entries come from this run's PASS logs)"
    echo
    grep -q -- '--- PASS: TestWorkloadReconcilerLocalLifecycle' "$log_dir/P-03-controller-lifecycle.log"
    echo "Accepted     PASS  Workload validation and reconcile lifecycle"
    grep -q -- '--- PASS: TestLocalFirstSelect' "$log_dir/P-02-policy-matrix.log"
    echo "Selected     PASS  Local/Remote(mock)/Auto policy selection"
    grep -q -- '--- PASS: TestLocalAllocatorIsIdempotentForLeaseKey' "$log_dir/P-01-core-unit-tests.log"
    echo "Reserved     PASS  Lease-key idempotent reservation"
    grep -q -- '--- PASS: TestBuildRuntimePlanSupportsConfiguredLocalResourceForms' "$log_dir/P-01-core-unit-tests.log"
    echo "RuntimeReady PASS  RuntimePlan generation for configured resource forms"
    grep -q -- '--- PASS: TestWorkloadAndLeaseDeletionReleaseRemoteCapacity' "$log_dir/P-04-conflict-boundary-release.log"
    echo "Released     PASS  Workload/Lease deletion releases simulated capacity"
    echo
    echo "Boundary: this matrix proves control-plane logic only."
    echo "It does not prove that a container or a real remote GPU task ran."
} >"$log_dir/P-07-control-chain-matrix.log" 2>&1

echo "Platform adapter evidence completed: $evidence_root"
