#!/usr/bin/env bash
set -euo pipefail

example_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repository_root="$(cd -- "$example_root/../.." && pwd)"
runtime_source="${CRICKET_CLIENT_DIR:-/home/qcrs/cricket-client}"
runtime_image="${RUNTIME_IMAGE:-cricket-client:5d55bd5}"
workload_image="${WORKLOAD_IMAGE:-cricket-remote-kernel-test:cuda12.8}"
manager_image="${MANAGER_IMAGE:-accelerator-control-plane:dev}"

install -d "$example_root/runtime/bin"
install -m 0755 "$runtime_source/cricket-client.so" \
  "$example_root/runtime/bin/cricket-client.so"
install -m 0755 "$runtime_source/libtirpc.so.3.0.0" \
  "$example_root/runtime/bin/libtirpc.so.3.0.0"
cp -f "$example_root/runtime/bin/libtirpc.so.3.0.0" \
  "$example_root/runtime/bin/libtirpc.so.3"
cp -f "$example_root/runtime/bin/libtirpc.so.3.0.0" \
  "$example_root/runtime/bin/libtirpc.so"

docker build -t "$runtime_image" "$example_root/runtime"
docker build -t "$workload_image" "$example_root/app"
docker build -t "$manager_image" "$repository_root"

if [[ -n "${KIND_CLUSTER:-}" ]]; then
  kind load docker-image --name "$KIND_CLUSTER" \
    "$runtime_image" "$workload_image" "$manager_image"
fi

docker image inspect \
  --format '{{.Id}} {{json .RepoTags}}' \
  "$runtime_image" "$workload_image" "$manager_image"
