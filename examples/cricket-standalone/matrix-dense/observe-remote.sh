#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../../.." && pwd)"
evidence_root="$repo_root/docs/evidence/cricket-standalone"
log_dir="$evidence_root/logs"
runs="${MATRIX_OBSERVE_RUNS:-20}"

mkdir -p "$log_dir"
gpu_log="$log_dir/C-07-matrix-gpu-dmon.log"
summary_log="$log_dir/C-07-matrix-run-summary.log"

echo "starting remote GPU monitor"
ssh cricket-remote "nvidia-smi dmon -s u -d 1 -c 15" >"$gpu_log" 2>&1 &
monitor_pid=$!

passed=0
failed=0
: >"$summary_log"
for run in $(seq 1 "$runs"); do
  run_log="$log_dir/C-07-matrix-run-$(printf '%02d' "$run").log"
  if "$script_dir/run-remote.sh" >"$run_log" 2>&1; then
    echo "run=$(printf '%02d' "$run") result=PASS" | tee -a "$summary_log"
    passed=$((passed + 1))
  else
    echo "run=$(printf '%02d' "$run") result=FAIL" | tee -a "$summary_log"
    failed=$((failed + 1))
  fi
done

wait "$monitor_pid"
{
  echo "passed=$passed"
  echo "failed=$failed"
  if [[ "$failed" -eq 0 ]]; then
    echo "overall=PASS"
  else
    echo "overall=FAIL"
  fi
  echo "gpu_log=$gpu_log"
} | tee -a "$summary_log"

[[ "$failed" -eq 0 ]]
