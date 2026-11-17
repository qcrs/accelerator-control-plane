#!/usr/bin/env bash
set -euo pipefail

cluster_name="${KIND_CLUSTER:-cricket-e2e}"
node_image="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"

if kind get clusters | grep -Fxq "$cluster_name"; then
  echo "kind cluster already exists: $cluster_name"
  exit 0
fi

kind create cluster --name "$cluster_name" --image "$node_image" --wait 180s
kubectl cluster-info --context "kind-$cluster_name"
