# S1 Control-Plane Design

## Component ownership

`AcceleratorProvider`, `AcceleratorPool`, and `AcceleratorEndpoint` are
cluster-scoped configuration. `AcceleratorWorkload` and `AcceleratorLease` are
namespaced so the Workload can own its Lease and Pod through normal Kubernetes
owner references.

There is no custom remote Agent:

- `cricket-rpc-server` is the existing remote CUDA data-plane process.
- `AcceleratorEndpoint.spec` declares how the controller reaches that process
  and which devices it exposes.
- `AcceleratorLease` is the durable capacity and cleanup record.
- The Workload controller is the only writer of allocation Lease
  specifications.

Endpoint status is intentionally optional. A separate health observer may set
the standard `Ready` condition and `lastProbeTime`; when the condition is
absent, a structurally valid static endpoint remains schedulable. When it is
present and not `True`, new allocations are rejected.

## Selection and accounting

Inventory readers normalize each local Node or remote Endpoint into a
candidate. Candidates are filtered by readiness, device kind, provider driver,
vendor, model, labels, memory, free capacity, and the selected Provider's
runtime-specific endpoint validator. Auto placement defaults to Local then
Remote. Within each location, candidates sort by descending Pool priority,
Pool name, best-fit free capacity, and stable candidate ID.

A local request must fit on one Node because one Pod is scheduled to one Node.
A remote Pool chooses one of two allocation modes:

- `ExclusiveEndpoint`: one Lease owns the complete server endpoint. The
  workload may request fewer devices than the server exposes, but no other
  workload can use the remaining devices.
- `Device`: individual devices may be assigned independently. A Provider must
  explicitly support safe per-device isolation before using this mode.

The Cricket Provider accepts only `ExclusiveEndpoint` and exactly one selected
endpoint. This matches a statically deployed server whose
`CUDA_VISIBLE_DEVICES` cannot be changed per client session.

Local availability is Node allocatable minus non-terminal Pod requests and
active local Leases. A bound local Lease stops being counted separately only
after its Pod is observed on the selected Node. Remote availability excludes
devices and endpoints held by every non-terminal Lease. A newly created Lease
with an empty status phase also consumes capacity, closing the create/status
initialization race.

Every Lease records the source Workload generation in its immutable spec. If
Lease creation succeeds but the Workload status update is lost, reconciliation
finds the deterministic Lease name first and reconstructs status from that
Lease. It never writes a newly computed decision over an existing allocation.
If the Workload spec changed during that window, the stale Lease is released
and a new allocation attempt is started.

## Interface boundaries

`InventoryReader`
: Produces normalized candidates. Kubernetes is one implementation.

`PolicyEngine`
: Selects a Provider, Pool, and ordered allocation targets without knowing
  runtime injection details.

`Allocator`
: Reserves, binds, and releases capacity. The static remote allocator resolves
  declarative endpoints and creates immutable binding snapshots. A future
  external session manager can replace this allocator without changing the
  controller.

`EndpointResolver`
: Resolves provider-neutral endpoint configuration. The current implementation
  reads `AcceleratorEndpoint` objects.

`RemoteEndpointValidator`
: Validates provider-specific endpoint address, runtime identity, and
  parameters. The Provider registry dispatches validation by runtime driver;
  both inventory discovery and reservation use the same contract.

`Provider`
: Validates provider/pool configuration and converts a bound Lease into a
  transport-independent `RuntimePlan`.

`PodInjector`
: Idempotently applies a `RuntimePlan`, rejecting conflicts with
  accelerator-managed environment variables, volumes, and resources.

## Immutable remote binding

At reservation time the allocator copies each selected endpoint name, address,
runtime identity, and provider parameters into
`AcceleratorLease.status.binding`. Pod injection
uses this snapshot rather than rereading mutable endpoint configuration. An
address change therefore cannot silently redirect an already selected
workload. The controller reports such a change as degraded while leaving the
S1 workload in place.

## Failure behavior

- A capacity conflict before Pod creation closes the current attempt and
  triggers a new selection attempt.
- Invalid or unsupported endpoint configuration is rejected before it can
  become a binding and the failed attempt stops consuming capacity.
- Reservation TTL starts at Lease creation and applies until a Pod is bound.
  If a Pod was created but binding did not finish, expiry deletes that Pod
  before releasing capacity.
- Endpoint deletion, `Ready=False`, Pool/address/runtime/parameter changes, or
  selected-device health changes mark a running workload degraded; S1 does not
  migrate it.
- Pod creation failure releases the central reservation immediately.
- A permanent RuntimePlan or Pod-mutation error releases the reservation and
  leaves `RuntimeReady=False` on both the Lease and Workload.
- Pod termination or Workload deletion transitions the Lease through release
  before removing its finalizer.
- Direct deletion of a Lease first removes any associated non-terminal Pod and
  leaves the Workload terminal; capacity is never freed while that Pod can
  still use the endpoint.
- Static Cricket release has no remote side effect. Removing the durable Lease
  releases its centrally accounted endpoint capacity.

## Extension points

The contracts can host an NPU Provider, a different GPU-over-IP Provider, a
remote health observer, a dynamic session allocator, fractional-device
inventory, or a Kubernetes DRA allocator. Dynamic process lifecycle is not
part of the Cricket S1 adapter and must not be hidden behind a nominally
implemented interface.
