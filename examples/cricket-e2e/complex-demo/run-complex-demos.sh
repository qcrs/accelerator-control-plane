#!/usr/bin/env bash
set -eo pipefail

example_root="$(cd -- "$(dirname -- "$0")" && pwd)"

for spec in "$example_root"/workloads/remote-*.yaml; do
  workload_name="$(sed -n '/^  name:/ { s/^  name: //; p; q; }' "$spec")"
  echo
  echo "=== applying platform workload: $workload_name ==="
  kubectl -n default delete acceleratorworkload "$workload_name" \
    --ignore-not-found --wait=true --timeout=120s
  kubectl apply -f "$spec"

  kubectl -n default wait "acceleratorworkload/$workload_name" \
    --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s
  pod_name="$(kubectl -n default get acceleratorworkload "$workload_name" \
    -o jsonpath='{.status.podRef.name}')"
  lease_name="$(kubectl -n default get acceleratorworkload "$workload_name" \
    -o jsonpath='{.status.allocationRef}')"
  remote_address="$(kubectl -n default get pod "$pod_name" \
    -o jsonpath='{.spec.containers[?(@.name=="worker")].env[?(@.name=="REMOTE_GPU_ADDRESS")].value}')"
  preload_path="$(kubectl -n default get pod "$pod_name" \
    -o jsonpath='{.spec.containers[?(@.name=="worker")].env[?(@.name=="LD_PRELOAD")].value}')"
  if [ -z "$remote_address" ] || [ -z "$preload_path" ]; then
    echo "Cricket runtime injection is missing for $workload_name" >&2
    exit 1
  fi

  kubectl -n default get acceleratorworkload "$workload_name" -o wide
  kubectl -n default get pod "$pod_name" --show-labels
  echo "injected REMOTE_GPU_ADDRESS: $remote_address"
  echo "injected LD_PRELOAD: $preload_path"
  kubectl -n default logs "$pod_name" -c worker

  if [ -n "$lease_name" ]; then
    kubectl -n default wait "acceleratorlease/$lease_name" \
      --for=jsonpath='{.status.phase}'=Released --timeout=120s
  fi
  echo "=== completed platform workload: $workload_name ==="
done
