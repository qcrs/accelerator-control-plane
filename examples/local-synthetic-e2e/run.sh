#!/usr/bin/env bash
set -euo pipefail

example_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
node_name="${SYNTHETIC_LOCAL_NODE:-}"
provider_name="synthetic-local-provider"
pool_name="synthetic-local-pool"
workload_name="synthetic-local-adapter"
resource_path="example.com~1synthetic-gpu"
node_label="accelerator.qcrs.io/synthetic-local-test"

if [[ -z "$node_name" ]]; then
  node_name="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
fi
if [[ -z "$node_name" ]]; then
  echo "no Kubernetes Node was found" >&2
  exit 2
fi

cleanup() (
  set +e
  kubectl -n default delete "acceleratorworkload/$workload_name" \
    --ignore-not-found --wait=true --timeout=120s
  kubectl delete "acceleratorpool/$pool_name" --ignore-not-found --wait=true --timeout=120s
  kubectl delete "acceleratorprovider/$provider_name" --ignore-not-found --wait=true --timeout=120s
  kubectl label node "$node_name" "$node_label-" >/dev/null 2>&1 || true
  kubectl patch node "$node_name" --subresource=status --type=json \
    -p="[{\"op\":\"remove\",\"path\":\"/status/capacity/$resource_path\"},{\"op\":\"remove\",\"path\":\"/status/allocatable/$resource_path\"}]" \
    >/dev/null 2>&1 || true
)
trap cleanup EXIT

# Reset only resources owned by this synthetic test, then publish exactly one
# temporary extended resource. This emulates Device Plugin scheduling capacity;
# it never claims that the kind node contains a physical GPU.
cleanup
kubectl label node "$node_name" "$node_label=true" --overwrite
kubectl patch node "$node_name" --subresource=status --type=json \
  -p="[{\"op\":\"add\",\"path\":\"/status/capacity/$resource_path\",\"value\":\"1\"},{\"op\":\"add\",\"path\":\"/status/allocatable/$resource_path\",\"value\":\"1\"}]"

kubectl apply -f "$example_root/provider-pool.yaml"
kubectl wait "acceleratorprovider/$provider_name" --for=condition=Ready --timeout=120s
kubectl wait "acceleratorpool/$pool_name" --for=condition=Ready --timeout=120s

kubectl apply -f "$example_root/workload.yaml"
kubectl -n default wait "acceleratorworkload/$workload_name" \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s

pod_name="$(kubectl -n default get "acceleratorworkload/$workload_name" \
  -o jsonpath='{.status.podRef.name}')"
lease_name="$(kubectl -n default get "acceleratorworkload/$workload_name" \
  -o jsonpath='{.status.allocationRef}')"
selected_mode="$(kubectl -n default get "acceleratorworkload/$workload_name" \
  -o jsonpath='{.status.selectedMode}')"
pod_node="$(kubectl -n default get pod "$pod_name" -o jsonpath='{.spec.nodeName}')"
resource_contract="$(kubectl -n default get pod "$pod_name" \
  -o jsonpath='{.spec.containers[?(@.name=="worker")].resources}')"
remote_address="$(kubectl -n default get pod "$pod_name" \
  -o jsonpath='{.spec.containers[?(@.name=="worker")].env[?(@.name=="REMOTE_GPU_ADDRESS")].value}')"
rpc_id="$(kubectl -n default get pod "$pod_name" \
  -o jsonpath='{.spec.containers[?(@.name=="worker")].env[?(@.name=="CRICKET_RPCID")].value}')"
preload_path="$(kubectl -n default get pod "$pod_name" \
  -o jsonpath='{.spec.containers[?(@.name=="worker")].env[?(@.name=="LD_PRELOAD")].value}')"
init_containers="$(kubectl -n default get pod "$pod_name" -o jsonpath='{.spec.initContainers[*].name}')"

if [[ "$selected_mode" != "Local" || "$pod_node" != "$node_name" ]]; then
  echo "expected Local selection on $node_name, got mode=$selected_mode node=$pod_node" >&2
  exit 1
fi
if [[ "$resource_contract" != *"example.com/synthetic-gpu"* ]]; then
  echo "local extended resource was not injected: $resource_contract" >&2
  exit 1
fi
if [[ -n "$remote_address" || -n "$rpc_id" || -n "$preload_path" || -n "$init_containers" ]]; then
  echo "Local runtime unexpectedly contains Cricket injection" >&2
  exit 1
fi

kubectl -n default get "acceleratorworkload/$workload_name" -o wide
kubectl -n default get pod "$pod_name" --show-labels
echo "selectedMode: $selected_mode"
echo "bound node: $pod_node"
echo "injected local resources: $resource_contract"
echo "Cricket variables: REMOTE_GPU_ADDRESS=${remote_address:-unset} CRICKET_RPCID=${rpc_id:-unset} LD_PRELOAD=${preload_path:-unset}"
kubectl -n default logs "$pod_name" -c worker

if [[ -n "$lease_name" ]]; then
  kubectl -n default wait "acceleratorlease/$lease_name" \
    --for=jsonpath='{.status.phase}'=Released --timeout=120s
fi
echo "synthetic LocalOnly adapter container test completed"
