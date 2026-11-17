#!/usr/bin/env bash
set -eo pipefail

example_root="$(cd -- "$(dirname -- "$0")" && pwd)"
repository_root="$(cd -- "$example_root/../../.." && pwd)"

echo "=== control-plane adapter and lifecycle tests ==="
(
  cd "$repository_root"
  go_test_mode="${GO_TEST_MODE:-auto}"
  go_test_image="${GO_TEST_IMAGE:-golang:1.26.5-bookworm}"
  go_proxy="${GOPROXY:-https://goproxy.cn,direct}"
  go_test_packages=(./...)
  if [ "$go_test_mode" = "skip" ]; then
    echo "GO_TEST_MODE=skip: Go unit tests were not run"
  elif command -v go >/dev/null 2>&1; then
    GOPROXY="$go_proxy" go test "${go_test_packages[@]}" -count=1
  elif [ "$go_test_mode" = "host" ]; then
    echo "GO_TEST_MODE=host requires Go in PATH" >&2
    exit 2
  elif [ "$go_test_mode" = "container" ] || [ "$go_test_mode" = "auto" ]; then
    if ! command -v docker >/dev/null 2>&1; then
      echo "Go is not installed and Docker is unavailable; set up Go or use GO_TEST_MODE=skip" >&2
      exit 2
    fi
    docker run --rm \
      -e "GOPROXY=$go_proxy" \
      -v "$repository_root:/workspace" \
      -w /workspace \
      "$go_test_image" \
      go test "${go_test_packages[@]}" -count=1
  else
    echo "unknown GO_TEST_MODE=$go_test_mode (expected auto, host, container, or skip)" >&2
    exit 2
  fi
)

echo "=== CRD server-side validation for complex workloads ==="
for spec in "$example_root"/workloads/*.yaml; do
  workload_name="$(sed -n '/^  name:/ { s/^  name: //; p; q; }' "$spec")"
  schema_name="${workload_name}-schema-check"
  sed "0,/^  name: ${workload_name}$/s//  name: ${schema_name}/" "$spec" \
    | kubectl create --dry-run=server -f -
done

echo "=== RemoteOnly platform workloads ==="
"$example_root/run-complex-demos.sh"

run_auto="$RUN_AUTO"
if [ -z "$run_auto" ]; then
  run_auto="false"
fi
if [ "$run_auto" = "true" ]; then
  echo "=== Auto platform workload ==="
  kubectl -n default delete acceleratorworkload platform-auto-image-pipeline \
    --ignore-not-found --wait=true --timeout=120s
  kubectl apply -f "$example_root/workloads/platform-auto-image-pipeline.yaml"
  kubectl -n default wait acceleratorworkload/platform-auto-image-pipeline \
    --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s
  auto_pod="$(kubectl -n default get acceleratorworkload platform-auto-image-pipeline \
    -o jsonpath='{.status.podRef.name}')"
  auto_lease="$(kubectl -n default get acceleratorworkload platform-auto-image-pipeline \
    -o jsonpath='{.status.allocationRef}')"
  auto_mode="$(kubectl -n default get acceleratorworkload platform-auto-image-pipeline \
    -o jsonpath='{.status.selectedMode}')"
  kubectl -n default get acceleratorworkload platform-auto-image-pipeline -o wide
  echo "Auto selectedMode: $auto_mode"
  kubectl -n default get pod "$auto_pod" --show-labels
  kubectl -n default logs "$auto_pod" -c worker
  if [ -n "$auto_lease" ]; then
    kubectl -n default wait "acceleratorlease/$auto_lease" \
      --for=jsonpath='{.status.phase}'=Released --timeout=120s
  fi
fi

run_local="$RUN_LOCAL"
if [ -z "$run_local" ]; then
  run_local="false"
fi
if [ "$run_local" = "true" ]; then
  echo "=== LocalOnly platform workload ==="
  kubectl -n default delete acceleratorworkload platform-local-image-pipeline \
    --ignore-not-found --wait=true --timeout=120s
  kubectl apply -f "$example_root/workloads/platform-local-image-pipeline.yaml"
  kubectl -n default wait acceleratorworkload/platform-local-image-pipeline \
    --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s
  local_pod="$(kubectl -n default get acceleratorworkload platform-local-image-pipeline \
    -o jsonpath='{.status.podRef.name}')"
  local_lease="$(kubectl -n default get acceleratorworkload platform-local-image-pipeline \
    -o jsonpath='{.status.allocationRef}')"
  local_mode="$(kubectl -n default get acceleratorworkload platform-local-image-pipeline \
    -o jsonpath='{.status.selectedMode}')"
  kubectl -n default get acceleratorworkload platform-local-image-pipeline -o wide
  echo "LocalOnly selectedMode: $local_mode"
  kubectl -n default get pod "$local_pod" --show-labels
  kubectl -n default logs "$local_pod" -c worker
  if [ -n "$local_lease" ]; then
    kubectl -n default wait "acceleratorlease/$local_lease" \
      --for=jsonpath='{.status.phase}'=Released --timeout=120s
  fi
fi
