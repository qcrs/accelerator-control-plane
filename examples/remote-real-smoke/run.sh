#!/usr/bin/env bash
set -euo pipefail

# Run one real remote GPU/NPU workload. The client machine only needs Docker,
# kubectl and the image artifacts; device access is required by the remote
# server node, never by this shell or the local Kubernetes node.
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
driver="${REMOTE_DRIVER:-corex}"
namespace="${NAMESPACE:-default}"
evidence="${EVIDENCE_DIR:-$root/docs/evidence/remote-real-smoke/$driver}"

case "$driver" in
  corex) manifest="$root/config/samples/corex-remote.yaml"; server="$root/config/samples/corex-remote-server.yaml"; workload=corex-remote-smoke; image="corex-workload:arm64" ;;
  mlu) manifest="$root/config/samples/mlu-remote.yaml"; server="$root/config/samples/mlu-remote-server.yaml"; workload=mlu-remote-smoke; image="mlu-workload:arm64" ;;
  *) echo "REMOTE_DRIVER must be corex or mlu" >&2; exit 2 ;;
esac

mkdir -p "$evidence"
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 2; }
if command -v docker >/dev/null; then
  docker image inspect "$image" >/dev/null 2>&1 || {
    echo "missing local image $image; build/load the real workload image first" >&2; exit 3;
  }
fi

if [[ "${DEPLOY_SERVER:-false}" == true ]]; then
  kubectl apply -f "$server"
fi
# Workload specs become immutable once allocation starts. Recreate only the
# smoke object owned by this script so repeated runs always exercise a fresh
# reservation/session lifecycle.
kubectl -n "$namespace" delete "acceleratorworkload/$workload" --ignore-not-found --wait=true --timeout=120s
kubectl apply -f "$manifest"
deadline=$((SECONDS + ${TIMEOUT:-300}))
while (( SECONDS < deadline )); do
  phase="$(kubectl -n "$namespace" get "acceleratorworkload/$workload" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  case "$phase" in
    Succeeded) break ;;
    Failed) echo "remote $driver smoke failed" >&2; break ;;
  esac
  sleep 2
done

kubectl -n "$namespace" get "acceleratorworkload/$workload" -o yaml >"$evidence/workload.yaml"
lease="$(kubectl -n "$namespace" get "acceleratorworkload/$workload" -o jsonpath='{.status.allocationRef}')"
pod="$(kubectl -n "$namespace" get "acceleratorworkload/$workload" -o jsonpath='{.status.podRef.name}')"
[[ -n "$lease" ]] && kubectl -n "$namespace" get "acceleratorlease/$lease" -o yaml >"$evidence/lease.yaml" || true
[[ -n "$pod" ]] && kubectl -n "$namespace" describe pod "$pod" >"$evidence/pod-describe.txt"
[[ -n "$pod" ]] && kubectl -n "$namespace" logs "$pod" -c worker >"$evidence/client.stdout" 2>"$evidence/client.stderr" || true

if [[ -n "$pod" ]]; then
  kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="worker")].state.terminated.exitCode}' >"$evidence/exit-code"
  test "$(cat "$evidence/exit-code")" = 0
fi
echo "remote $driver smoke succeeded; evidence: $evidence"
