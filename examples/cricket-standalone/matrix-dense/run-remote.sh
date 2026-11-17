#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../../.." && pwd)"
evidence_root="$repo_root/docs/evidence/cricket-standalone"
bin_dir="$evidence_root/bin"
result_dir="$evidence_root/results"
remote_address="${REMOTE_GPU_ADDRESS:-192.168.5.6:53773}"
seed="${MATRIX_SEED:-20260820}"
report="$result_dir/C-06-matrix-dense-report.txt"

mkdir -p "$result_dir"

echo "caller: $(whoami)@$(hostname)"
echo "REMOTE_GPU_ADDRESS=$remote_address"
echo "CRICKET_RPCID=1"
echo "program=$bin_dir/cricket-matrix-dense"
echo "workload: Y = ReLU(X x W + bias), matrices are 512x512"
echo "local seed=$seed"

set +e
timeout 120s env \
  REMOTE_GPU_ADDRESS="$remote_address" \
  CRICKET_RPCID=1 \
  MATRIX_SEED="$seed" \
  MATRIX_REPORT_PATH="$report" \
  LD_PRELOAD="$bin_dir/cricket-client.so" \
  LD_LIBRARY_PATH="$bin_dir" \
  "$bin_dir/cricket-matrix-dense"
rc=$?
set -e

echo "exit=$rc"
if [[ -f "$report" ]]; then
  echo "report=$report"
  sha256sum "$report"
fi
exit "$rc"
