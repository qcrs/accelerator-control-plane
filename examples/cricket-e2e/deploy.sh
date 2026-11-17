#!/usr/bin/env bash
set -euo pipefail

example_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repository_root="$(cd -- "$example_root/../.." && pwd)"
endpoint_host="${CRICKET_ENDPOINT_HOST:-192.168.5.6}"
endpoint_port="${CRICKET_ENDPOINT_PORT:-53773}"

if ! timeout 5s bash -c "</dev/tcp/$endpoint_host/$endpoint_port"; then
  echo "Cricket endpoint is unreachable: $endpoint_host:$endpoint_port" >&2
  exit 1
fi

kubectl apply --server-side --force-conflicts \
  -k "$repository_root/config/default"
kubectl -n accelerator-system rollout status \
  deployment/accelerator-manager \
  --timeout=180s

sample_workload="remote-kernel-test"
# AcceleratorWorkload.spec becomes immutable once allocation starts. This
# example manifest deliberately contains a runnable smoke Workload, so make
# repeated demo deployments reset only that named example before applying it.
kubectl -n default delete "acceleratorworkload/$sample_workload" \
  --ignore-not-found --wait=true --timeout=120s

kubectl apply -f \
  "$repository_root/config/samples/remote-cricket-5060ti.yaml"

kubectl -n default wait "acceleratorworkload/$sample_workload" \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
sample_lease="$(kubectl -n default get "acceleratorworkload/$sample_workload" \
  -o jsonpath='{.status.allocationRef}')"
if [[ -n "$sample_lease" ]]; then
  kubectl -n default wait "acceleratorlease/$sample_lease" \
    --for=jsonpath='{.status.phase}'=Released --timeout=120s
fi

kubectl get acceleratorprovider,acceleratorpool,acceleratorendpoint
kubectl -n default get acceleratorworkload,acceleratorlease,pod
