# Cricket Integration Contract

## Pinned upstream

- Repository: `https://github.com/RWTH-ACS/cricket.git`
- Cricket commit: `5d55bd582fc7acbb2a12c45f104020566a67218a`
- Cricket license: MIT
- `libtirpc` submodule commit:
  `5ca4ca92f629d9d83e83544b9239abaaacf0a527`
- The locked `libtirpc` source uses a permissive three-clause BSD-style
  license. Cricket is MIT, but its complete dependency set is not
  "MIT-only".

The upstream source is cloned next to this repository at
`/home/qcrs/vGPU/cricket`. The original submodule host returned HTTP 403, so
the exact locked commit was obtained from the public
`https://github.com/alisw/libtirpc.git` mirror. No upstream source was modified.

## Client image contract

This control plane does not build or publish Cricket. The image referenced by
`AcceleratorProvider.spec.clientRuntime.image` must contain the complete
Cricket `build/bin` output at `sourcePath` (default `/opt/cricket/bin`),
including at least:

- `cricket-client.so`
- `libtirpc.so`
- `libtirpc.so.3`

The image must also provide `/bin/cp` with support for `-a`; the generated init
container uses it to materialize the runtime into the shared volume.

The Provider copies that directory into an `emptyDir`, mounts it into the
workload container, and injects:

```text
REMOTE_GPU_ADDRESS=<bound endpoint address>
CRICKET_RPCID=<optional endpoint rpcID>
LD_PRELOAD=<mountPath>/cricket-client.so
LD_LIBRARY_PATH=<mountPath>:<existing value>
```

Existing `LD_PRELOAD` and `LD_LIBRARY_PATH` values are preserved after the
managed Cricket paths.

## Server contract

The first supported deployment model is a preinstalled server on a remote GPU
host. Start one server process for each independently allocatable endpoint and
restrict its visible devices before startup:

```bash
CUDA_VISIBLE_DEVICES=0 /opt/cricket/bin/cricket-rpc-server 1
```

The trailing value is the RPC version and must match endpoint parameter
`rpcID`. When `rpcID` is omitted, the client uses version `1`. If
`runtime.apiVersion` is declared, the control plane requires it to match that
effective RPC ID. Cricket registers its TCP service through `rpcbind`;
therefore the host and rpcbind-selected service port must be reachable from
workload Pods. Normally `spec.address` is just a host or IPv4 address. The
client supports an explicit `host:port`, but upstream does not expose a server
CLI option that binds a stable fixed port.

An `AcceleratorEndpoint` must declare exactly what that server can see:

```yaml
spec:
  address: gpu-a.example
  runtime:
    driver: cricket
    version: 5d55bd582fc7acbb2a12c45f104020566a67218a
    apiVersion: "1"
  parameters:
    rpcID: "1"
  devices:
    - id: gpu-0
      vendor: nvidia
      model: A100
      memory: 40Gi
      health: Healthy
```

Changing `CUDA_VISIBLE_DEVICES` or restarting the server with another RPC ID
requires updating the Endpoint only for future allocations. Existing Leases
retain their original binding snapshot.

The Cricket Provider rejects unknown endpoint parameters, malformed or IPv6
addresses, mismatched `runtime.apiVersion`/`rpcID`, and client runtime paths
that would be hidden by the init container's `/runtime` volume mount. The same
validator runs while building inventory and again immediately before the
binding snapshot is created.

## Compatibility and security limits

- CUDA applications must link CUDA APIs dynamically; CUDA runtime linkage may
  require `-cudart shared`.
- Upstream documents PyTorch support as experimental and requires a custom
  framework build.
- The current client uses IPv4 resolver structures and does not correctly
  parse IPv6 endpoint literals.
- Cricket provides neither TLS nor client authentication. Use a private
  network, firewall rules, and Kubernetes NetworkPolicy where applicable.
- A Kubernetes Service cannot directly model Cricket's default random RPC
  service port. Container-managed server deployment needs a separate fixed-port
  or host-network adapter and is outside this S1 implementation.
- Endpoint health and inventory are declarations unless an independent
  observer publishes the optional `Ready` condition.
