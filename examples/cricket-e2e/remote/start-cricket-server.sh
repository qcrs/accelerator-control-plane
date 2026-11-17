#!/usr/bin/env bash
set -euo pipefail

cricket_root="${1:-/home/pc/src/cricket}"
server="$cricket_root/bin/cricket-rpc-server"
log_file="$cricket_root/cricket-server.log"

if [[ ! -x "$server" ]]; then
    echo "Cricket server executable was not found: $server" >&2
    exit 1
fi

while IFS= read -r pid; do
    [[ -n "$pid" ]] && kill "$pid"
done < <(pgrep -f "^$server 1$" || true)

export CRICKET_LIBDIR="$cricket_root/submodules/libtirpc/install/lib"
export LD_LIBRARY_PATH="$cricket_root/bin:$CRICKET_LIBDIR:/usr/local/cuda/lib64:/usr/lib/wsl/lib:/usr/lib/x86_64-linux-gnu"

nohup env CUDA_VISIBLE_DEVICES=0 "$server" 1 >"$log_file" 2>&1 </dev/null &
server_pid=$!

sleep 1
if ! kill -0 "$server_pid" 2>/dev/null; then
    echo "Cricket server exited during startup." >&2
    tail -n 80 "$log_file" >&2 || true
    exit 1
fi

echo "$server_pid"
