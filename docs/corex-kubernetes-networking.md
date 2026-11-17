# CoreX Kubernetes 网络路径

CoreX Remote client 原先在 `connect_server()` 中使用 `inet_pton(AF_INET, ...)`，只接受数字 IPv4 地址。MLU client 使用 `getaddrinfo()`，因此可以直接使用 Kubernetes DNS。

这会导致 CoreX 使用 Service/StatefulSet DNS 时在 client 初始化阶段报 `cuInit(0) failed: driver not initialized`，而同一 client 连接 Pod IP 可以正常完成 H2D、kernel、同步、D2H 和 cleanup。

根因已在 `corex-remote-runtime/src/corex_remote_cuda.c` 修复：CoreX client 现在使用 `getaddrinfo()`，逐个尝试解析地址并设置 `TCP_NODELAY`。控制面仍使用 StatefulSet + headless Service，使 Endpoint 地址稳定；Provider 的 DNS 解析到 Pod IP 逻辑作为兼容层保留，直到所有部署都使用修复后的 CoreX client 镜像。

验证路径：

```text
corex-remote-server-0 StatefulSet
  -> headless Service DNS
  -> CoreX getaddrinfo()
  -> CRX9/TCP session
  -> H2D/kernel/sync/D2H
  -> Lease Released
```

当前真实 K3s smoke 已通过，证据位于 `docs/evidence/remote-real-smoke/corex/`。
