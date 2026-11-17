# S2 Startup Fallback and Configuration Health

## Scope

This increment extends S1 placement without changing either accelerator data
plane. Local execution still uses the Kubernetes device plugin and remote CUDA
still uses the existing MIT-licensed Cricket client/server. There is no custom
Agent, remote NPU runtime, checkpoint restore, or live migration in this
design. The local control plane can target configured whole-device, MIG,
shared, or custom extended resources; their actual Device Plugin and isolation
behavior remains external.

The implemented production-facing control-plane additions are:

1. bounded fallback before a generated Pod first becomes Ready;
2. durable retry history and deadlines;
3. failed node/Endpoint exclusion with opposite-mode preference;
4. Kubernetes Event audit records;
5. static Provider, Pool, and Endpoint configuration health;
6. durable pre-Pod fallback for retryable reservation and runtime-preparation
   conflicts.

## Workload API

```yaml
spec:
  placement:
    mode: Auto
    order: [Local, Remote]
    fallback:
      enabled: true
      maxAttempts: 2       # total attempts, including the first
      startupTimeout: 5m   # measured from Pod creation
      backoff: 5s
```

Fallback requires Auto placement and both execution modes. Defaults are two
total attempts, a five-minute startup deadline, and five seconds of backoff.
The supported attempt range is two through eight.

`status.fallback` contains:

- `failedAttempts`: durable retry-budget consumption;
- `readyObserved`: monotonic proof that fallback is no longer permitted;
- `startupStartedAt`: the current Pod startup clock;
- `requeueAt`: an absolute backoff deadline;
- `history`: up to eight immutable failure summaries.

Each history entry records the allocation attempt, mode, Provider, Pool,
targets, Pod identity, start/failure timestamps, reason, and message. The
controller derives failed-target exclusions from history instead of keeping an
in-memory deny list.

## Failure policy

The eligibility boundary is whether this Pod has ever produced
`PodReady=True`. Before that boundary the controller immediately recognizes
scheduler unschedulability, accelerator-managed runtime init failures, startup
Endpoint invalidation, and the configured timeout. Before a Pod exists, an
Allocator `Conflict`/`Unschedulable` result or the same retryable result from
Provider RuntimePlan preparation is also eligible. The controller persists a
history entry with no Pod reference, releases the Lease, applies the same
backoff, and prefers the opposite mode.

Application image pull failures, user init-container failures, and application
process exits are not immediately replayed on another target: changing the GPU
location normally cannot fix them and replay may duplicate side effects. The
runtime injector annotates the exact init containers it added, so runtime image
or setup failures can be distinguished from user containers. A finite
`startupTimeout` remains an explicit broad retry boundary chosen by the
operator.

After that boundary, a process exit is an application result. It releases the
Lease and becomes `Succeeded` or `Failed`; it is never replayed as a fresh
workload. A user-deleted/lost Pod and permanent Workload, Provider, or Pod
mutation error are also excluded from automated fallback. Invalid or
unsupported RuntimePlans and conflicting accelerator-managed Pod template
material are permanent failures, not retryable runtime-preparation conflicts.

## Recoverable state machine

```text
Starting
  -> classify a pre-Ready or pre-Pod retryable failure
  -> persist history and CleanupPending
  -> delete old Pod and wait for NotFound, when a Pod exists
  -> release Lease and remove its finalizer
  -> persist RequeueAt and clear active allocation
  -> select again, preferring the opposite mode
  -> exclude every node/Endpoint in prior failure history
  -> reserve, inject, create, and bind a new Pod
```

The status write precedes destructive cleanup. If the manager restarts after
Pod deletion or Lease release, the recorded attempt resumes at the next
idempotent step. Capacity is never released while the old Pod object can still
use its remote binding.

## Selection behavior

The policy contract accepts a soft `PreferredMode` and hard candidate
exclusions. The local-first policy applies both after normal compatibility and
health filtering:

- the opposite mode is moved to the front of the configured Auto order;
- a failed local node is not selected again;
- every failed remote Endpoint is not selected again;
- other candidates in the same mode remain eligible if the preferred mode has
  no capacity.

This preserves availability without retrying the exact target that already
failed. Capacity races and retryable runtime-preparation conflicts before Pod
creation consume one durable startup-fallback attempt so repeated conflicts
cannot reset the budget across reconciles or manager restarts.

## Configuration health

Three independent reconcilers validate declarative configuration and requeue
dependents when their references change:

| Object | Condition | Static checks |
| --- | --- | --- |
| Provider | `Ready` | registered driver, unique supported device kinds, TLS/auth capability, driver-specific client configuration |
| Pool | `Ready` | Provider reference, location, device kind, resource form/allocation mode, selector, Provider compatibility |
| Endpoint | `Configured` | remote Pool reference, runtime driver, supported device resource forms, address/parameters, unique device IDs and UUIDs |

Endpoint `Configured` intentionally differs from Endpoint `Ready`. A future
network observer owns `Ready` and `lastProbeTime`; the static controller cannot
overwrite live reachability. Inventory rejects an explicitly false condition
of either type and rejects stale `observedGeneration` values while continuing
to support structurally valid objects with no live condition during initial
reconciliation.

## Audit contract

Durable status is authoritative for recovery. Events are the operator-facing
audit stream and cover `Selected`, `FallbackSelected`, `Reserved`,
`PodCreated`, `Bound`, `Ready`, `StartupFailed`, `FallbackScheduled`,
`FallbackExhausted`, `Degraded`, `Released`, `ConfigurationReady`, and
`ConfigurationInvalid` transitions.

## Deferred work

- active Endpoint network probes and latency-aware scoring;
- authentication or encrypted transport for Cricket;
- namespace quotas and admission policy;
- Prometheus metrics and SLO alerts;
- dynamic remote server/session lifecycle;
- checkpoint recovery and post-Ready failover;
- MIG creation/reconfiguration, HAMi or other sharing-runtime enforcement, and
  physical-device UUID selection;
- real Device Plugin/hardware validation, CUDA execution, and NPU data-plane
  implementations.

These require new runtime or operational authority and are not represented by
placeholder implementations in the current interfaces.
