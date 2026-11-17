#!/usr/bin/env bash
set -euo pipefail

example_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

kubectl -n default delete acceleratorworkload/remote-cricket-demo \
  --ignore-not-found --wait=true --timeout=120s
kubectl apply -f "$example_root/demo-workload.yaml"
kubectl -n default wait acceleratorworkload/remote-cricket-demo \
  --for=jsonpath='{.status.phase}'=Succeeded \
  --timeout=180s

pod_name="$(kubectl -n default get acceleratorworkload remote-cricket-demo \
  -o jsonpath='{.status.podRef.name}')"
if [[ -z "$pod_name" ]]; then
  echo "The demo workload did not publish a Pod reference." >&2
  exit 1
fi

kubectl -n default get acceleratorworkload remote-cricket-demo -o wide
kubectl -n default get acceleratorlease
kubectl -n default get pod "$pod_name" --show-labels
kubectl -n default logs "$pod_name" -c worker
