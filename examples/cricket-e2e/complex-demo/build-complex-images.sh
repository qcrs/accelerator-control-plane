#!/usr/bin/env bash
set -eo pipefail

example_root="$(cd -- "$(dirname -- "$0")" && pwd)"
complex_image="$COMPLEX_IMAGE"
if [ -z "$complex_image" ]; then
  complex_image="cricket-image-demos:cuda12.8"
fi

docker build -t "$complex_image" "$example_root/app"

if [ -n "$KIND_CLUSTER" ]; then
  kind load docker-image --name "$KIND_CLUSTER" "$complex_image"
fi

docker image inspect --format '{{.Id}} {{json .RepoTags}}' "$complex_image"
