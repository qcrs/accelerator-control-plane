# Accelerator Control Plane

This project implements startup-time local or remote accelerator selection and
bounded fallback before an application Pod first becomes Ready.
Applications submit an `AcceleratorWorkload`; the controller selects capacity,
creates a durable `AcceleratorLease`, injects the selected runtime, and creates
one concrete Pod.

## Implemented

- Cluster-scoped Provider, Pool, and declarative remote Endpoint APIs.
- Namespaced Workload and allocation Lease APIs.
- Kubernetes Node/Pod and static remote runtime inventory normalization.
- Deterministic local-first selection with central capacity accounting.
- A runtime-agnostic `ExecutionAdapter` registry that keeps each Provider and
  Allocator on one validated driver boundary.
- Generic local extended-resource discovery, exact Kubernetes
  `resource.Quantity` accounting, and requests/limits injection for
  `WholeDevice`, `MIG`, `Shared`, and `Custom` resource forms.
- MIT-licensed Cricket remote-CUDA client injection through a pluggable
  Provider. Its bundled libtirpc dependency uses a permissive BSD-style
  license.
- Endpoint-exclusive allocation for remote runtimes without per-device tenant
  isolation.
- API-server atomic endpoint claims backed by `coordination.k8s.io/Lease`, so
  stale caches and leader failover cannot double-allocate a Cricket server.
- Immutable local resource/target snapshots and remote endpoint bindings in
  each Lease.
- Persistent S2 startup-failure history, bounded cross-mode fallback, retry
  deadlines, and failed-target exclusion.
- Kubernetes Events for selection, reservation, Pod creation, binding,
  startup failure, fallback, degradation, release, and configuration health.
- Static `Ready` conditions for Provider/Pool configuration and a separate
  Endpoint `Configured` condition that does not conflict with live `Ready`.
- Live Pool capacity summaries derived from Nodes, Pods, Endpoints, and active
  allocation Leases.
- Reproducible CRD generation, a non-root container image, RBAC (including
  leader election), and Kustomize deployment manifests.

## Architecture boundary

The control plane does not implement CUDA RPC and does not deploy a custom
remote Agent. Cricket supplies `cricket-client.so` and
`cricket-rpc-server`. This repository owns only selection, reservation,
runtime materialization, and Pod mutation.

Remote endpoints are declared in Kubernetes. The endpoint `spec` contains the
server address, runtime version, visible devices, and non-secret connection
parameters. Built-in reconcilers publish a structurally validated `Configured`
condition. An independent observer may add live `Ready` network health and own
`lastProbeTime`; static validation does not probe a server.

Published Provider/Pool `Ready` and Endpoint `Configured`/`Ready` conditions
are trusted only when their `observedGeneration` matches the current object.

## Deliberate boundaries

- S1 startup-time selection and opt-in S2 pre-Ready fallback are implemented.
  There is no post-Ready application restart, checkpoint recovery, or live
  migration.
- Local control-plane semantics cover configured whole-device, MIG, shared,
  and custom extended resource names. Accounting includes application
  containers, the maximum init-container request, Pod overhead, and active
  Lease reservations using exact quantities.
- The controller does not create or reconfigure MIG instances, implement HAMi
  virtualization, select physical device UUIDs, or enforce isolation itself.
  Local Device Plugin behavior and NPU data planes still require a target
  cluster; the Cricket remote CUDA path is exercised by the real platform
  container E2E under `examples/cricket-e2e/complex-demo`.
- Remote NPU runtimes and remote fractional-device/session isolation remain
  extension points. The built-in Cricket adapter exposes only whole GPUs.
- Cricket endpoints are allocated exclusively. A request never aggregates
  multiple Cricket servers into one client process, and its requested device
  count must equal the healthy devices visible to that static server process.
- The Cricket client image and endpoint must declare the same exact
  `runtimeVersion`; mismatches fail before Pod creation.
- Cricket requires dynamically linked CUDA libraries. Framework compatibility
  is not implied by successful injection.
- Cricket has no built-in TLS or authentication. Network isolation is an
  operator responsibility.
- Arbitrary Pods are not intercepted. Applications submit an
  `AcceleratorWorkload` containing a `PodTemplateSpec`.

## API flow

```text
AcceleratorWorkload
  -> normalized local and remote inventory
  -> local-first policy decision
  -> AcceleratorLease (immutable targets; capacity is accounted immediately)
  -> ExecutionAdapter reserve + optional immutable remote endpoint binding
  -> mode-checked Provider RuntimePlan + Pod mutation
  -> Pod + bound Lease
  -> central capacity release when the Pod terminates
```

The Kubernetes scheduler and configured device plugin remain authoritative for
local capacity. Local reservations are counted across every Pool that shares
the same extended resource name. A Lease snapshots that name and its resource
form so later Pool edits or deletion cannot hide the reservation. For remote
capacity, every non-terminal `AcceleratorLease` is counted immediately and the
final endpoint ownership decision is an atomic coordination Lease create,
independent of cache freshness.

Lease creation is retry-safe: each Lease records the Workload generation used
for selection, and a reconciliation retry restores its decision from the
existing immutable Lease instead of selecting a different target under the
same allocation name.

With `spec.placement.fallback.enabled: true`, only failures observed before the
generated Pod has ever reached `Ready=True` are eligible for fallback. The
controller records each failed attempt in Workload status, deletes the old Pod,
waits for deletion, releases its Lease, applies a durable backoff deadline, and
then prefers the other execution mode. Failed nodes or endpoints are excluded
from later attempts. `maxAttempts` includes the initial attempt.

Application image errors and application process exits are not immediately
replayed on another accelerator target. Immediate fallback is reserved for
scheduler/endpoint failures and accelerator-managed runtime setup failures;
retryable capacity/runtime-preparation conflicts before Pod creation are also
recorded before their Lease is released. The configured startup timeout
remains the explicit broad retry boundary.

## Build, test, and deploy

```bash
go test ./...
go vet ./...
go build -trimpath -o bin/manager ./cmd/manager
docker build -t accelerator-control-plane:dev .
kubectl apply --server-side --force-conflicts -k config/default
```

For the complete adapter/container validation (full Go tests, CRD server-side
dry-run, three multi-stage CUDA image workloads, and optional Auto/LocalOnly
variants), see
[examples/cricket-e2e/complex-demo/README.zh-CN.md](examples/cricket-e2e/complex-demo/README.zh-CN.md):

```bash
GO_TEST_MODE=container RUN_AUTO=true \
  ./examples/cricket-e2e/complex-demo/run-platform-matrix.sh
```

The current kind + Windows/WSL demonstration has passed the three RemoteOnly
jobs and the Auto job on a freshly rebuilt controller image. A separate
[synthetic LocalOnly adapter container check](examples/local-synthetic-e2e/README.zh-CN.md)
also verifies local resource injection without Cricket runtime injection; it
does not replace a physical NVIDIA Device Plugin/GPU acceptance test.

The envtest suite additionally starts a real API server and etcd, installs the
checked-in CRDs, verifies defaulting, and exercises atomic endpoint claims:

```bash
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.36.2 -p path)"
make envtest
```

On this Windows workspace, dependency and test-asset downloads use
`HTTP_PROXY`/`HTTPS_PROXY=http://127.0.0.1:7890`. From NAT-mode WSL2, use the
Windows host gateway instead of WSL localhost:

```bash
HOST_IP="$(ip route | awk '/default/ {print $3; exit}')"
export HTTP_PROXY="http://${HOST_IP}:7890"
export HTTPS_PROXY="http://${HOST_IP}:7890"
```

See [docs/design.md](docs/design.md),
[docs/s2-startup-fallback.md](docs/s2-startup-fallback.md),
[docs/cricket-integration.md](docs/cricket-integration.md),
[docs/wsl-development-environment.zh-CN.md](docs/wsl-development-environment.zh-CN.md),
[docs/repository-delivery-status.zh-CN.md](docs/repository-delivery-status.zh-CN.md),
[docs/platform-adapter-implementation.zh-CN.md](docs/platform-adapter-implementation.zh-CN.md),
[docs/cricket-standalone-remote-call.zh-CN.md](docs/cricket-standalone-remote-call.zh-CN.md),
[docs/implementation-roadmap.zh-CN.md](docs/implementation-roadmap.zh-CN.md), and
`config/samples`.
