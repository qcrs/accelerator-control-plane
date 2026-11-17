# S1/S2 Control-Plane Design

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
present and not `True`, new allocations are rejected. A published condition is
also rejected when its `observedGeneration` is stale.

The built-in configuration reconcilers publish Provider/Pool `Ready` and
Endpoint `Configured` from static validation. They check registered drivers,
Provider capabilities, references, Pool/runtime compatibility, Endpoint
addresses and parameters, and unique device IDs/UUIDs. `Configured` is kept
separate from Endpoint `Ready` so a live observer has one unambiguous condition
to own. Static validation does not set `lastProbeTime` or probe a server.

## Selection and accounting

Inventory readers normalize each local Node or remote Endpoint into a
candidate. Candidates are filtered by readiness, device kind, provider driver,
vendor, model, labels, memory, resource form, profile, isolation, requested
attributes, free capacity, and the selected Provider's runtime-specific
endpoint validator. Auto placement defaults to Local then Remote. Within each
location, candidates sort by descending Pool priority, Pool name, best-fit free
capacity, and stable candidate ID.

A local request must fit on one Node because one Pod is scheduled to one Node.
A remote Pool chooses one of two allocation modes:

- `ExclusiveEndpoint`: one Lease owns the complete server endpoint. The
  selected device count must equal all healthy devices exposed by the static
  runtime. This prevents a client from seeing GPUs that were not allocated.
- `Device`: individual devices may be assigned independently. A Provider must
  explicitly support safe per-device isolation before using this mode.

The Cricket Provider accepts only `ExclusiveEndpoint` and exactly one selected
endpoint. This matches a statically deployed server whose
`CUDA_VISIBLE_DEVICES` cannot be changed per client session.

Local availability is Node allocatable minus non-terminal Pod requests and
active local Leases across all Pools sharing the same extended resource. Pod
usage follows Kubernetes scheduling semantics with exact `resource.Quantity`
arithmetic: application-container requests are summed, the largest
init-container request is compared with that sum, and Pod overhead is added. A
bound local Lease stops being counted separately only after its Pod is observed
on the selected Node actually requesting the snapshotted target resource. The
Lease snapshot remains authoritative if its Pool is edited or deleted. Remote
availability excludes devices and endpoints held by every non-terminal Lease.
A newly created Lease with an empty status phase also consumes capacity,
closing the create/status initialization race.

Pool status is recomputed from Node, Pod, Endpoint, and Lease watches. For
local Pools, `reserved` represents consumed capacity (scheduled Pod requests
plus unbound reservations). For remote Pools, it represents devices held by
active Leases; unavailable or unhealthy devices may therefore make
`total != available + reserved`.

Every Lease records both the source Workload generation and the selected
ExecutionAdapter driver in its immutable spec. If Lease creation succeeds but
the Workload status update is lost, reconciliation finds the deterministic
Lease name first and reconstructs status from that Lease. It never writes a
newly computed decision over an existing allocation. If the Workload spec
changed during that window, the stale Lease is released and a new allocation
attempt is started. Reserve and Bind still validate the live Provider/Pool;
Release resolves the Allocator from the immutable driver snapshot so deleting
configuration cannot strand an allocation finalizer. Failed or expired
historical Leases whose release finalizer is already absent are not released a
second time, because their former target may have a newer owner.

## Interface boundaries

`ExecutionAdapter`
: Registers one immutable Provider/Allocator pair under a driver name. The
  registry validates modes, device kinds, resource forms, and remote allocation
  modes at startup, rejects incomplete or duplicate adapters, and is the sole
  execution path used by the Workload controller.

`InventoryReader`
: Produces normalized candidates. Kubernetes is one implementation.

`PolicyEngine`
: Selects a Provider, Pool, and ordered allocation targets without knowing
  runtime injection details.

`Allocator`
: Reserves, binds, and releases capacity. The static remote allocator resolves
  declarative endpoints, atomically creates a coordination Lease per endpoint,
  and creates immutable binding snapshots. A future external session manager
  can replace this allocator as part of another ExecutionAdapter without
  changing the controller.

`EndpointResolver`
: Resolves provider-neutral endpoint configuration. The current implementation
  reads `AcceleratorEndpoint` objects.

`RemoteEndpointValidator`
: Validates provider-specific endpoint address, runtime identity, and
  parameters. The Provider registry dispatches validation by runtime driver;
  both inventory discovery and reservation use the same contract.

`Provider`
: Validates provider/pool configuration and converts a bound Lease into a
  transport-independent `RuntimePlan`. Every plan declares its execution mode;
  Local plans must request the selected Node/resource and Remote plans must not
  request any managed local extended resource.

`PodInjector`
: Idempotently applies a `RuntimePlan`, rejecting conflicts with
  accelerator-managed environment variables, volumes, and resources.

## Adding an execution adapter

All adapters are registered as one Provider/Allocator pair. Registering a
Provider separately is insufficient: the Workload controller resolves the
pair from `AcceleratorProvider.spec.driver` through the Adapter registry.

For another local Device Plugin adapter:

1. Implement `Provider.Driver`, declare `Local` plus the supported device
   kinds and resource forms, and validate the Provider/Pool configuration.
   Device Plugins with standard extended-resource semantics can reuse
   `local.NewWithDriver`.
2. Implement an idempotent Allocator. It owns the control-plane reservation;
   kube-scheduler, kubelet, and the Device Plugin continue to choose the
   physical device.
3. Return a Local RuntimePlan containing only the snapshotted resource
   requests/limits and selected Node constraint. Do not inject a remote shim,
   Endpoint, or physical UUID.
4. Register the pair with `adapter.NewBinding` during manager setup, then
   create a Provider and Local Pool whose `driver` and `resourceName` match the
   implementation. No Workload state-machine or LocalFirst policy change is
   required.

For another remote runtime adapter:

1. Implement a Provider that declares `Remote`, its supported resource forms,
   allocation modes, TLS/auth capabilities, and any provider-specific
   configuration or Endpoint validation.
2. Implement an idempotent Allocator that owns Endpoint/session reservation,
   immutable binding snapshots, binding, and release. An external session
   manager belongs behind this Allocator rather than in the controller.
3. Build a typed Remote RuntimePlan from the Lease binding. Runtime-specific
   filenames, paths, environment variables, ports, and protocol identifiers
   remain inside this Provider.
4. Register the Provider and Allocator as one Binding. Inventory normalization
   and the generic policy then select it without adding runtime-specific
   branches to the controller.

Every new adapter needs registration/error tests, capability/configuration
tests, RuntimePlan conflict and mode-boundary tests, and at least one
controller path proving the Adapter can reserve, materialize, bind, and
release its selected mode.

## Immutable allocation snapshots

For Local decisions, each Lease target snapshots the Node, extended resource
name, count, resource form, profile, isolation, and attributes selected by the
policy. Inventory accounting and RuntimePlan construction prefer this snapshot
over mutable Pool configuration. Local runtime materialization only injects
that extended resource and Node constraint; it never injects remote runtime
material.

At reservation time the allocator copies each selected endpoint name, address,
runtime identity, and provider parameters into
`AcceleratorLease.status.binding`. Pod injection
uses this snapshot rather than rereading mutable endpoint configuration. An
address change therefore cannot silently redirect an already selected
workload. The controller reports such a change as degraded while leaving the
S1 workload in place.

Endpoint ownership is separate from that snapshot. A stable, hashed
`coordination.k8s.io/Lease` name is created in the controller namespace. Its
holder is the allocation Lease UID and its annotations point back to that
allocation object. Create is the compare-and-swap operation; a live different
holder is a conflict, while a missing or terminal owner makes the claim
recyclable. Release checks the holder and uses UID/resourceVersion
preconditions before deletion.

## Failure behavior

- A capacity conflict before Pod creation closes the current attempt and
  triggers a durable S2 attempt when fallback is enabled; without S2 it returns
  to normal selection without creating a Pod.
- A retryable conflict or unschedulable result while building a RuntimePlan is
  handled as the same pre-Pod S2 failure. Invalid or unsupported plans and Pod
  template conflicts are permanent failures rather than fallback requests.
- Invalid or unsupported endpoint configuration is rejected before it can
  become a binding and the failed attempt stops consuming capacity.
- Reservation TTL starts at Lease creation and applies until a Pod is bound.
  If a Pod was created but binding did not finish, expiry deletes that Pod
  before releasing capacity.
- Endpoint deletion, `Ready=False`, Pool/address/runtime/parameter changes, or
  selected-device health changes mark a running workload degraded; S1 does not
  migrate it.
- A Pod create error that proves API rejection releases the reservation. A
  timeout or transport failure has an unknown commit result, so the controller
  retains the reservation and retries the deterministic Pod name. If the TTL
  later expires, an already-created Pod is deleted before capacity is released.
- A permanent RuntimePlan or Pod-mutation error releases the reservation and
  leaves `RuntimeReady=False` on both the Lease and Workload.
- Successful runtime injection, Pod creation, and binding publish
  `RuntimeReady=True` on both objects.
- Pod termination or Workload deletion transitions the Lease through release
  before removing its finalizer.
- Direct deletion of a Lease first removes any associated non-terminal Pod and
  leaves the Workload terminal; capacity is never freed while that Pod can
  still use the endpoint.
- Static Cricket release has no remote side effect. Removing the durable Lease
  releases its centrally accounted endpoint capacity.

## S2 startup fallback

Fallback is opt-in under `spec.placement.fallback` and is valid only for Auto
placement with both Local and Remote enabled. `maxAttempts` counts the initial
attempt; `startupTimeout` bounds each attempt until the Pod first reaches
Ready; `backoff` is persisted as an absolute `status.fallback.requeueAt`
deadline.

The controller classifies only target/runtime failures before Ready as
immediately fallback eligible:

- allocator capacity conflicts or unschedulable results before Pod creation;
- retryable capacity/runtime conflicts while preparing a RuntimePlan;
- scheduler `Unschedulable`;
- image pull, container creation, crash-loop, or non-zero exit failures in an
  accelerator-managed runtime init container;
- selected remote Endpoint invalidation during startup;
- expiry of `startupTimeout`.

Once a Pod has reached Ready, application termination and runtime degradation
remain terminal/degraded behavior and never trigger automatic replacement.
Before Ready, an application container image error or application process exit
is also not immediately replayed on another target. User deletion, a lost Pod,
and permanent Workload or Provider configuration errors are not interpreted as
fallback requests. Operators that want a broad retry boundary opt into a
finite `startupTimeout`.

Each failed startup is appended to `status.fallback.history` with its mode,
Provider, Pool, immutable targets, Pod reference, timestamps, reason, and
message. A retry reconstructs hard exclusions from the complete history and
softly prefers the opposite mode. State is preserved across manager restarts
and cannot reset the attempt budget.

Cleanup is deliberately ordered:

```text
record failure in Workload status
  -> delete the old Pod and wait until it no longer exists
  -> release the old Lease
  -> persist backoff and clear the active allocation
  -> select another mode while excluding failed targets
```

This ordering prevents a remote Endpoint from being reassigned while the old
client Pod can still issue calls.

## Audit events

Kubernetes Events complement, but never replace, durable status. The manager
records selection/fallback selection, reservation, Pod creation, binding,
Ready, startup failure, fallback scheduling/exhaustion, degradation, capacity
release, and configuration readiness transitions. Workload status and Lease
status remain the authoritative recovery state.

## Extension points

The contracts can host an NPU Provider, a different GPU-over-IP Provider, a
remote health observer, a dynamic session allocator, or a Kubernetes DRA
allocator. The generic Local adapter already models configured Kubernetes
extended resources as `WholeDevice`, `MIG`, `Shared`, or `Custom`, including
profile/isolation/attribute matching and exact quantity accounting. It does not
create MIG instances, implement a sharing runtime, assign physical UUIDs,
enforce isolation, or verify real Device Plugin/hardware behavior. Dynamic
process lifecycle is not part of the Cricket S1 adapter and must not be hidden
behind a nominally implemented interface.
