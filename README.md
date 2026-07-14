# Accelerator Control Plane

This project implements the S1 control-plane interfaces for startup-time local
or remote accelerator selection. An `AcceleratorWorkload` is the canonical
entry point; the controller selects capacity, creates an `AcceleratorLease`,
and then creates one concrete Pod.

## Implemented

- Cluster-scoped Provider, Pool, and Endpoint APIs.
- Namespaced Workload and allocation Lease APIs.
- Kubernetes Node/Pod and Remote Agent inventory normalization.
- Deterministic local-first selection and lease state transitions.
- Local extended-resource injection and Lupine client injection.
- Agent heartbeat and inventory reporting skeletons.
- Provider, allocator, Agent transport, probe, and session provisioner
  extension interfaces.

## Deliberate boundaries

- Startup-time S1 selection only. There is no fallback after a Pod starts,
  checkpoint recovery, or live migration.
- Whole-device allocation only. MIG, fractional GPU, and NPU runtimes are
  extension points.
- The Agent RPC transport, hardware probe, and per-lease Lupine process or
  container provisioner are not implemented. Unsupported placeholders fail
  explicitly.
- Cross-namespace trust-bundle copying is not implemented. A referenced CA
  Secret must currently be in the Workload namespace.
- Arbitrary Pods are not intercepted. Applications submit an
  `AcceleratorWorkload` containing a `PodTemplateSpec`.

## API flow

```text
AcceleratorWorkload
  -> inventory snapshot
  -> local-first policy decision
  -> AcceleratorLease (Pending -> Reserved)
  -> provider RuntimePlan + Pod mutation
  -> Pod + bound Lease
  -> release when Pod terminates or Workload is deleted
```

The controller uses the Kubernetes device-plugin resource as the final local
capacity gate. Remote capacity is only authoritative when a Remote Agent
implements atomic, idempotent `Reserve` and returns endpoints whose visible
devices are restricted to that lease.

See [docs/design.md](docs/design.md) for interface and failure semantics and
`config/samples` for API examples.
