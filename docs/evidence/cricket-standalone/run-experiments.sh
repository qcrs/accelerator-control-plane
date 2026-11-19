#!/usr/bin/env bash
set -euo pipefail

evidence_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin_dir="$evidence_root/bin"
log_dir="$evidence_root/logs"
result_dir="$evidence_root/results"
remote_address="192.168.5.6:53773"

mkdir -p "$log_dir" "$result_dir"

run_cricket() {
    local program="$1"
    shift
    timeout 120s env \
        REMOTE_GPU_ADDRESS="$remote_address" \
        CRICKET_RPCID=1 \
        LD_PRELOAD="$bin_dir/cricket-client.so" \
        LD_LIBRARY_PATH="$bin_dir" \
        "$bin_dir/$program" "$@"
}

{
    echo "evidence timestamp: $(date -Is)"
    echo "[deployed startup script]"
    ssh cricket-remote 'sha256sum /mnt/c/cricket-remote/start-cricket.ps1'
    echo
    echo "[current WSL service state]"
    ssh cricket-remote "hostname -I; pgrep -af 'sshd|rpcbind|cricket-rpc-server'; rpcinfo -p 127.0.0.1 | grep -E 'program|^[[:space:]]*99[[:space:]]+1[[:space:]]+tcp'"
    echo
    echo "[Windows fixed portproxy state]"
    ssh cricket-remote '/mnt/c/Windows/System32/netsh.exe interface portproxy show v4tov4'
} >"$log_dir/C-01-remote-recovery-state.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "caller hostname: $(hostname)"
    echo "caller user: $(whoami)"
    echo "remote endpoint: $remote_address"
    echo
    echo "[fixed TCP endpoints]"
    timeout 5s bash -c '</dev/tcp/192.168.5.6/53773'
    echo "Cricket TCP 53773: reachable"
    timeout 5s bash -c '</dev/tcp/192.168.5.6/2222'
    echo "SSH TCP 2222: reachable"
    echo
    echo "[remote identity and GPU]"
    ssh cricket-remote 'date -Is; hostname; whoami; hostname -I; nvidia-smi -L; nvcc --version'
} >"$log_dir/C-03-network-and-identity.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "[remote processes]"
    ssh cricket-remote "pgrep -af 'sshd|rpcbind|cricket-rpc-server'"
    echo
    echo "[remote RPC registry]"
    ssh cricket-remote 'rpcinfo -p 127.0.0.1'
    echo
    echo "[remote listening sockets]"
    ssh cricket-remote "ss -lntp | grep -E '(:22|:111|cricket-rpc)'"
} >"$log_dir/C-02-remote-rpc-registration.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "[artifact hashes]"
    sha256sum \
        "$bin_dir/cricket-client.so" \
        "$bin_dir/cricket-smoke" \
        "$bin_dir/cricket-kernel-smoke" \
        "$bin_dir/cricket-image-pipeline" \
        "$bin_dir/libcudart.so.12.8.90"
    echo
    echo "[dynamic CUDA runtime]"
    LD_LIBRARY_PATH="$bin_dir" ldd "$bin_dir/cricket-smoke" | grep -E 'libcudart|not found'
    LD_LIBRARY_PATH="$bin_dir" ldd "$bin_dir/cricket-kernel-smoke" | grep -E 'libcudart|not found'
    LD_LIBRARY_PATH="$bin_dir" ldd "$bin_dir/cricket-image-pipeline" | grep -E 'libcudart|not found'
    echo
    echo "[Cricket client dependencies]"
    LD_LIBRARY_PATH="$bin_dir" ldd "$bin_dir/cricket-client.so" | grep -E 'libtirpc|not found'
} >"$log_dir/artifact-and-linkage.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "command: LD_PRELOAD=cricket-client.so REMOTE_GPU_ADDRESS=$remote_address cricket-smoke"
    run_cricket cricket-smoke
    rc=$?
    echo "exit=$rc"
} >"$log_dir/C-04-api-smoke.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "command: LD_PRELOAD=cricket-client.so REMOTE_GPU_ADDRESS=$remote_address cricket-kernel-smoke"
    run_cricket cricket-kernel-smoke
    rc=$?
    echo "exit=$rc"
} >"$log_dir/C-05-kernel-verification.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "command: LD_PRELOAD=cricket-client.so REMOTE_GPU_ADDRESS=$remote_address cricket-image-pipeline"
    OUTPUT_PATH="$result_dir/image-pipeline-edge.pgm" run_cricket cricket-image-pipeline
    rc=$?
    echo "exit=$rc"
    echo "output: $result_dir/image-pipeline-edge.pgm"
    sha256sum "$result_dir/image-pipeline-edge.pgm"
} >"$log_dir/C-06-image-pipeline.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "[GPU utilization sampled while 20 complex pipelines run]"
    ssh cricket-remote 'nvidia-smi dmon -s u -d 1 -c 12' >"$log_dir/C-07-gpu-dmon-raw.log" 2>&1 &
    monitor_pid=$!
    for run in $(seq 1 20); do
        OUTPUT_PATH="$result_dir/image-pipeline-edge.pgm" \
            run_cricket cricket-image-pipeline \
            >"$log_dir/C-07-load-run-$(printf '%02d' "$run").log" 2>&1
    done
    wait "$monitor_pid"
    cat "$log_dir/C-07-gpu-dmon-raw.log"
    echo
    echo "[server log after complex pipelines]"
    ssh cricket-remote 'tail -n 100 /home/pc/src/cricket/cricket-server.log'
    echo
    echo "[GPU identity after complex pipeline]"
    ssh cricket-remote 'nvidia-smi -L'
} >"$log_dir/C-07-remote-observation.log" 2>&1

{
    echo "evidence timestamp: $(date -Is)"
    echo "test: 20 sequential cross-machine kernel executions"
    passed=0
    for run in $(seq 1 20); do
        run_log="$log_dir/stability-run-$(printf '%02d' "$run").log"
        if run_cricket cricket-kernel-smoke >"$run_log" 2>&1 && \
            grep -q 'verification failures: 0' "$run_log"; then
            passed=$((passed + 1))
            printf 'run=%02d result=PASS\n' "$run"
        else
            printf 'run=%02d result=FAIL log=%s\n' "$run" "$run_log"
        fi
    done
    echo "passed=$passed"
    echo "failed=$((20 - passed))"
    if [[ "$passed" -ne 20 ]]; then
        exit 1
    fi
    echo "overall=PASS"
} >"$log_dir/C-08-stability-summary.log" 2>&1

echo "Cricket standalone evidence completed: $evidence_root"
