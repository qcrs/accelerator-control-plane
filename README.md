# Accelerator Control Plane

This project implements startup-time local or remote accelerator selection.
Applications submit an `AcceleratorWorkload`; the controller selects capacity,
creates a durable `AcceleratorLease`, injects the selected runtime, and creates
one concrete Pod.

## Implemented

- Cluster-scoped Provider, Pool, and declarative remote Endpoint APIs.
- Namespaced Workload and allocation Lease APIs.
- Kubernetes Node/Pod and static remote runtime inventory normalization.
- Deterministic local-first selection with central capacity accounting.
- Local device-plugin resource injection.
- MIT-licensed Cricket remote-CUDA client injection through a pluggable
  Provider. Its bundled libtirpc dependency uses a permissive BSD-style
  license.
- Endpoint-exclusive allocation for remote runtimes without per-device tenant
  isolation.
- Immutable endpoint snapshots in each Lease binding.

## Architecture boundary

The control plane does not implement CUDA RPC and does not deploy a custom
remote Agent. Cricket supplies `cricket-client.so` and
`cricket-rpc-server`. This repository owns only selection, reservation,
runtime materialization, and Pod mutation.

Remote endpoints are declared in Kubernetes. The endpoint `spec` contains the
server address, runtime version, visible devices, and non-secret connection
parameters. An optional independent observer may publish a `Ready` condition,
but no observer is required for a static S1 deployment.

## Deliberate boundaries

- S1 startup-time selection only. There is no restart fallback, checkpoint
  recovery, or live migration.
- Whole-device accounting only. MIG, fractional GPU, and NPU remote runtimes
  remain extension points.
- Cricket endpoints are allocated exclusively. A request never aggregates
  multiple Cricket servers into one client process.
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
  -> AcceleratorLease (capacity is accounted immediately)
  -> immutable remote endpoint binding
  -> Provider RuntimePlan + Pod mutation
  -> Pod + bound Lease
  -> central capacity release when the Pod terminates
```

The Kubernetes scheduler and configured device plugin remain authoritative for
local capacity. For remote capacity, the controller serializes decisions and
counts every non-terminal `AcceleratorLease`, including a newly created Lease
whose status has not yet been initialized.

Lease creation is retry-safe: each Lease records the Workload generation used
for selection, and a reconciliation retry restores its decision from the
existing immutable Lease instead of selecting a different target under the
same allocation name.

See [docs/design.md](docs/design.md),
[docs/cricket-integration.md](docs/cricket-integration.md), and
`config/samples`.
