#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../../.." && pwd)"
remote_host="${CRICKET_BUILD_HOST:-cricket-remote}"
remote_source="/tmp/cricket-matrix-dense.cu"
remote_binary="/tmp/cricket-matrix-dense"
local_binary="$repo_root/docs/evidence/cricket-standalone/bin/cricket-matrix-dense"

echo "build host: $remote_host"
echo "source: $script_dir/matrix_dense.cu"
scp "$script_dir/matrix_dense.cu" "$remote_host:$remote_source"
ssh "$remote_host" \
  "export PATH=/usr/local/cuda/bin:\$PATH; \
   export LD_LIBRARY_PATH=/usr/local/cuda/lib64:/usr/lib/wsl/lib:\${LD_LIBRARY_PATH:-}; \
   /usr/local/cuda/bin/nvcc -O3 -std=c++14 -cudart shared -arch=native \
     -I/usr/local/cuda/include '$remote_source' -o '$remote_binary'"
scp "$remote_host:$remote_binary" "$local_binary"
chmod +x "$local_binary"

echo "local artifact: $local_binary"
sha256sum "$local_binary"
LD_LIBRARY_PATH="$repo_root/docs/evidence/cricket-standalone/bin" \
  ldd "$local_binary" | grep -E 'libcudart|not found'
