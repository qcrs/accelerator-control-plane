# CoreX hostname 连接与 Kubernetes 地址

根因是旧 CoreX client 在 `connect_server()` 使用 `inet_pton(AF_INET, host, ...)`，只接受数字 IPv4。MLU client 使用 `getaddrinfo()`，因此支持 hostname。旧客户端会在连接前把 DNS 名称拒绝，上层只显示 `cuInit ... driver not initialized`。

当前 K3s 单后端已实测 CRX9 经 ClusterIP/NAT 计算成功。此前对比同时混入了旧 workload 镜像、错误 kernel 文件配对、客户端 hostname 支持等差异，不能据此得出 NAT 根因。本机 Docker image ID 标识 manifest，而 K3s Pod imageID 标识 config，应比较同一类 digest 或实际文件 SHA256。

本轮已删除 CoreX Provider 的 DNS→Pod IP 绕过。Endpoint 的稳定 DNS 原样注入 `COREX_REMOTE_HOST`，解析由 CoreX client 的 `getaddrinfo()` 完成。客户端逐地址连接，共享五秒连接预算，并报告 resolve/connect 错误；DNS 查询本身受系统 resolver 控制，不包含在这五秒内。不会在运行中的 session 断开后自动重放任务。

StatefulSet + headless Service 保留，用于单 Endpoint 的稳定身份，不再描述为 NAT 修复。server Pod 重建后，**新客户端连接**重新解析 DNS；已断开的运行中连接不会自动恢复，也不支持热迁移。

验证分为两层：

- 单元测试使用不存在的 StatefulSet DNS，证明 Provider 不进行预解析、原样传递名称。
- 单 ARM 真实实验：客户端的 `/dev` 和厂商 SDK 被隐藏；独立客户端及无设备 Docker workload 直接使用 `localhost` hostname，经 TCP 连接独立 server，完成 GPU kernel、CPU 数值比对、回收。没有经过 Provider DNS 改写。

以上是早期独立实验的证据边界。2026-11-19 已完成新版 K3s 的真实验收：server Pod IP 从 `10.42.0.109` 变为 `10.42.0.119` 后，原 StatefulSet DNS 不变，新任务直接使用域名完成 GPU 数值校验；另用普通 ClusterIP `10.43.28.184` 转发到该 Pod，客户端直接使用 Service DNS，同样完成真实 kernel、数值比对和资源回收。Provider 均未改写 DNS。

该结果证明当前单后端 CoreX 可以正常通过 ClusterIP/NAT；headless Service 是稳定实例身份的选择，不是协议必需的 NAT 绕过。测试结束已恢复原 Endpoint DNS、删除临时 Service。实际 Pod env、镜像 config digest、数值日志及 session 归零证据见 [K3s 验收记录](k3s-validation-report.zh-CN.md) 和其中的 `full-reliability/`。

本地源码补丁、构建和验收方法见 [运行时观察与测试闭环](remote-observation-and-validation.zh-CN.md)。
