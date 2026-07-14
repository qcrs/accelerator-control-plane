# S1 Control-Plane Design

## Resource ownership

`AcceleratorProvider`, `AcceleratorPool`, and `AcceleratorEndpoint` are
cluster-scoped. `AcceleratorWorkload` and `AcceleratorLease` are namespaced so
the Workload can own the Lease and Pod with normal Kubernetes owner references.
The Workload controller is the only writer of allocation Lease specifications.

Agent liveness and capacity ownership are intentionally different mechanisms:

- `coordination.k8s.io/Lease` is a 15-second heartbeat with a 45-second expiry.
- `AcceleratorLease` is a durable capacity record with owner UID, selected
  targets, binding receipt, and cleanup finalizer.

## Selection

Inventory readers normalize each local Node or remote Endpoint into a
candidate. Candidates are filtered by readiness, device kind, vendor, model,
labels, memory, and free capacity. Auto placement defaults to Local then
Remote. Within each location, candidates sort by descending Pool priority,
Pool name, best-fit free capacity, and stable candidate ID.

A local request must fit on one Node because a Pod is scheduled to one Node. A
remote request may consume multiple Endpoint targets from one Pool. Target and
device order is preserved through allocation and Lupine's `LUPINE_SERVER`
value.

Local availability is Node allocatable minus non-terminal Pod requests and
active local Leases. A Lease stops being counted separately only after its Pod
is observed on the selected Node, preventing double accounting. The
kube-scheduler and device plugin remain the authoritative local allocation
gate.

## Provider and Agent contracts

Providers validate configuration and produce a transport-independent
`RuntimePlan`. The shared injector applies the plan idempotently and rejects
reserved-name conflicts. Allocators must treat the Lease UID as an idempotency
key; retries after a controller crash must return the same reservation.

For Lupine, a static server that exposes all host GPUs is not a valid isolated
allocation. The Agent's `SessionProvisioner` must start or configure a
lease-scoped server with only selected devices visible, then return one or more
ordered bare, HTTP, or HTTPS endpoints. The current placeholder intentionally
returns `Unsupported` until that integration is supplied.

## Failure behavior

- A capacity conflict before Pod creation closes the current attempt and
  triggers a new selection attempt.
- A reservation TTL applies only before a Pod is bound.
- A stale Agent is removed from new decisions. A running S1 workload is marked
  degraded but is not moved.
- Pod creation failure releases the reservation immediately.
- Pod termination or Workload deletion releases remote sessions before the
  allocation finalizer is removed.
- Release failures retain the finalizer and capacity record. This prevents
  accidental reuse while a remote session may still exist.

## Future adapters

The same contracts can host an NPU Provider, a real Lupine Agent transport and
provisioner, fractional-device inventory, or a Kubernetes DRA allocator. DRA is
not the v1 persistence model because it would require node-side Prepare and
Unprepare plugins for both execution paths.
