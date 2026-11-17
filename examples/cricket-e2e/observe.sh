#!/usr/bin/env bash
set -euo pipefail

kubectl -n default wait acceleratorworkload/remote-kernel-test \
  --for=jsonpath='{.status.phase}'=Succeeded \
  --timeout=180s

pod_name="$(
  kubectl -n default get acceleratorworkload remote-kernel-test \
    -o jsonpath='{.status.podRef.name}'
)"

if [[ -z "$pod_name" ]]; then
  echo "The workload did not publish a Pod reference." >&2
  exit 1
fi

kubectl -n default get acceleratorworkload remote-kernel-test -o wide
kubectl -n default get acceleratorlease
kubectl -n default logs "$pod_name" -c worker
