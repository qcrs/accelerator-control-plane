#!/usr/bin/env bash
# Compile test artifacts in a separate directory; never replace dist/ or a
# running server. Both runtime repositories must include observation support.
set -euo pipefail
: "${COREX_ROOT:?set COREX_ROOT to the CoreX runtime checkout}"
: "${MLU_ROOT:?set MLU_ROOT to the MLU runtime checkout}"
: "${OUT_DIR:?set OUT_DIR to a validation artifact directory}"
corex_sdk="${COREX_SDK:-/usr/local/corex-4.4.0}"
mlu_sdk="${NEUWARE_HOME:-/usr/local/neuware-3.5.2}"
mkdir -p "$OUT_DIR/corex-lib"
cc="${CC:-gcc}"
# CoreX objects are rebuilt from source into OUT_DIR, never reused from dist.
mkdir -p "$OUT_DIR/obj"
for source in corex_runtime_context corex_launch_config corex_device_api corex_fatbin_runtime corex_metadata corex_driver_shim; do
 "$cc" -O2 -Wall -Wextra -Werror -std=gnu11 -fPIC -I"$corex_sdk/include" -I"$COREX_ROOT/include/internal" -I"$COREX_ROOT/include/generated" -pthread -c "$COREX_ROOT/src/$source.c" -o "$OUT_DIR/obj/$source.o"
done
"$cc" -O2 -Wall -Wextra -Werror -std=gnu11 -fPIC -I"$COREX_ROOT/include/internal" -I"$COREX_ROOT/include/generated" -pthread -c "$COREX_ROOT/src/corex_remote_cuda.c" -o "$OUT_DIR/corex_remote_cuda.o"
"$cc" -shared -Wl,-z,defs -Wl,-soname,libcorex_remote_cudart.so.1 -Wl,--version-script="$COREX_ROOT/packaging/corex_remote_cudart.map" \
 "$OUT_DIR/corex_remote_cuda.o" "$OUT_DIR/obj/corex_runtime_context.o" "$OUT_DIR/obj/corex_launch_config.o" "$OUT_DIR/obj/corex_device_api.o" "$OUT_DIR/obj/corex_fatbin_runtime.o" "$OUT_DIR/obj/corex_metadata.o" \
 -ldl -lm -pthread -o "$OUT_DIR/corex-lib/libcorex_remote_cudart.so.1.1.0"
"$cc" -shared -Wl,-z,defs -Wl,-soname,libcuda.so.1 -Wl,--version-script="$COREX_ROOT/packaging/corex_remote_cuda.map" "$OUT_DIR/obj/corex_driver_shim.o" -L"$OUT_DIR/corex-lib" -l:libcorex_remote_cudart.so.1.1.0 '-Wl,-rpath,$ORIGIN' -o "$OUT_DIR/corex-lib/libcorex_remote_cuda.so.1.0.0"
ln -sfn libcorex_remote_cudart.so.1.1.0 "$OUT_DIR/corex-lib/libcorex_remote_cudart.so.1"
ln -sfn libcorex_remote_cudart.so.1 "$OUT_DIR/corex-lib/libcorex_remote_cudart.so"
ln -sfn libcorex_remote_cuda.so.1.0.0 "$OUT_DIR/corex-lib/libcuda.so.1"
ln -sfn libcuda.so.1 "$OUT_DIR/corex-lib/libcuda.so"
"$cc" -O2 -Wall -Wextra -Werror -std=gnu11 -I"$corex_sdk/include" -I"$COREX_ROOT/include/internal" -I"$COREX_ROOT/include/generated" \
 "$COREX_ROOT/server/runtime_server.c" "$COREX_ROOT/server/corex_backend.c" "$COREX_ROOT/src/corex_metadata.c" \
 -L"$corex_sdk/lib64" -Wl,-rpath,"$corex_sdk/lib64" -lcuda -o "$OUT_DIR/corex-server"
"$cc" -O2 -I"$corex_sdk/include" "$COREX_ROOT/tests/driver/driver_smoke.c" -L"$OUT_DIR/corex-lib" -lcuda -o "$OUT_DIR/corex-smoke"
# MLU: a real BANG vector-add, not the memory-only client_basic smoke.
"$cc" -std=c11 -D_POSIX_C_SOURCE=200112L -O2 -fPIC -Wall -Wextra -Wno-unused-parameter -Werror=incompatible-pointer-types \
 -I"$MLU_ROOT/include" -I"$MLU_ROOT/build/generated" -I"$mlu_sdk/include" \
 "$MLU_ROOT/server/mlu_remote_server.c" "$MLU_ROOT/server/mlu_backend.c" "$MLU_ROOT/server/callback_bridge.c" "$MLU_ROOT/server/resource_limits.c" "$MLU_ROOT/server/object_registry.c" "$MLU_ROOT/server/server_session.c" "$MLU_ROOT/common/bang_image.c" \
 -L"$mlu_sdk/lib64" -Wl,-rpath,"$mlu_sdk/lib64" -lcndrv -lcnrt -lpthread -ldl -o "$OUT_DIR/mlu-server"
"$cc" -O2 -I"$MLU_ROOT/include" "$MLU_ROOT/tests/client_custom_kernel.c" -L"$MLU_ROOT/build" -lremote_cnrt -lpthread -o "$OUT_DIR/mlu-smoke"
