#!/usr/bin/env bash
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
driver="${REMOTE_DRIVER:-corex}"
namespace=default
# Samples explicitly use default; reject pretending a different namespace ran.
[[ "${NAMESPACE:-default}" == default ]] || { echo 'sample namespace is default' >&2; exit 2; }
case "$driver" in
  corex|mlu) ;;
  *) echo 'REMOTE_DRIVER must be corex or mlu' >&2; exit 2 ;;
esac
workload="$driver-remote-smoke"
endpoint="$driver-remote-server"
evidence="${EVIDENCE_DIR:-$root/docs/evidence/remote-real-smoke/$driver/$(date -u +%Y%m%dT%H%M%SZ)-$$}"
mkdir -p "$evidence"
# Always capture the failure scene, including API/log collection errors.
collect() {
  kubectl -n "$namespace" get "acceleratorworkload/$workload" -o json >"$evidence/workload.json" 2>"$evidence/workload.error" || true
  pod="$(kubectl -n "$namespace" get "acceleratorworkload/$workload" -o jsonpath='{.status.podRef.name}' 2>/dev/null || true)"
  lease="$(kubectl -n "$namespace" get "acceleratorworkload/$workload" -o jsonpath='{.status.allocationRef}' 2>/dev/null || true)"
  if [[ -n "$lease" ]]; then
    kubectl -n "$namespace" get "acceleratorlease/$lease" -o json >"$evidence/lease.json" 2>"$evidence/lease.error" || true
  fi
  if [[ -n "$pod" ]]; then
    kubectl -n "$namespace" get pod "$pod" -o json >"$evidence/pod.json" 2>"$evidence/pod.error" || true
    kubectl -n "$namespace" describe pod "$pod" >"$evidence/pod-describe.txt" 2>&1 || true
    kubectl -n "$namespace" logs "$pod" -c worker >"$evidence/client.stdout" 2>"$evidence/client.stderr" || true
  fi
  kubectl get "acceleratorendpoint/$endpoint" -o json >"$evidence/endpoint.json" 2>"$evidence/endpoint.error" || true
  kubectl -n accelerator-system logs -l "app.kubernetes.io/name=$endpoint" --all-containers=true --prefix --tail=-1 >"$evidence/server.log" 2>"$evidence/server.error" || true
  kubectl -n "$namespace" get events -o json >"$evidence/events.json" 2>"$evidence/events.error" || true
}
trap collect EXIT
if [[ "${DEPLOY_SERVER:-false}" == true ]]; then
  kubectl apply -f "$root/config/samples/$driver-remote-server.yaml"
fi
kubectl -n "$namespace" delete "acceleratorworkload/$workload" --ignore-not-found --wait=true --timeout=120s
kubectl apply -f "$root/config/samples/$driver-remote.yaml"
phase=''
deadline=$((SECONDS + ${TIMEOUT:-300}))
while (( SECONDS < deadline )); do
  phase="$(kubectl -n "$namespace" get "acceleratorworkload/$workload" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  case "$phase" in Succeeded|Failed) break ;; esac
  sleep 2
done
collect
[[ "$phase" == Succeeded ]] || { echo "FAIL: phase=${phase:-Unknown}; evidence=$evidence" >&2; exit 1; }
lease="$(kubectl -n "$namespace" get "acceleratorworkload/$workload" -o jsonpath='{.status.allocationRef}')"
[[ -n "$lease" ]] || { echo 'FAIL: missing Lease' >&2; exit 1; }
kubectl -n "$namespace" wait "acceleratorlease/$lease" --for=jsonpath='{.status.phase}'=Released --timeout=120s
# Endpoint observation can lag Lease release by one observation interval.
deadline=$((SECONDS + 45))
while :; do
  collect
  if python3 "$root/examples/remote-real-smoke/verify.py" "$evidence" "$driver" >"$evidence/result.txt" 2>&1; then break; fi
  if (( SECONDS >= deadline )); then cat "$evidence/result.txt" >&2; exit 1; fi
  sleep 2
done
trap - EXIT
cat "$evidence/result.txt"
echo "remote $driver smoke succeeded; evidence=$evidence"
