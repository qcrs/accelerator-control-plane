# 控制面四项闭环报告（2026-11-18）

## 结果

本轮完成本地实现、回归测试、独立真实硬件实验和六个 ARM64 镜像构建。没有部署当前 K3s、导入 containerd、提交 Git 或推送远端。现有未提交文件和原始资料保留。

| 项目 | 本轮结果 |
| --- | --- |
| 数值证据 | Smoke 要求明确的 CPU reference PASS 日志、Pod exit=0、Workload Succeeded、Lease Released 和最新 session 清理观察；Failed/超时/无 Pod/日志缺失不再误报 PASS |
| 真实健康/session | CoreX/MLU 服务端新增只读 observation 接口，复用实际 worker/清理计数；健康通过 native 设备枚举确认，区别于 kernel 验收 |
| CoreX DNS | 删除 Provider 预解析；客户端 getaddrinfo、逐地址尝试、共享五秒连接预算和错误诊断；真实无卡客户端直接 hostname 运行通过 |
| sidecar/分配/故障 | 正确统计 restartable init 重叠资源；只选择单一独占 Endpoint；补缺失/超时/过期/异常/会话残留/重启/Claim 后状态变化测试 |

## 修改文件与目的

| 文件或目录 | 目的 |
| --- | --- |
| `internal/inventory/accounting.go`、`accounting_test.go`、`lease_semantics_test.go` | 统一 Lease 消费口径；补 sidecar 的各阶段资源累计及 overhead |
| `internal/contracts/readiness.go`、`readiness_test.go` | 唯一的观察新鲜度/健康/session 就绪判断；Unknown、过期都不可分配 |
| `internal/contracts/registry.go`、`contracts.go` | 复用已有注册表和 EndpointObserver，在 Claim 后分派真实观察；不建立第二套 provider map |
| `internal/provider/observation/` | 有超时/大小/driver/schema/字段校验的只读 HTTP 观察器及错误测试；真实服务端读测入口 |
| `internal/provider/corex/provider.go`、`provider_test.go` | 删除 DNS→Pod IP 绕过，保留稳定 DNS；接入观察；禁止重新注入不必要的 LD_PRELOAD |
| `internal/provider/mlu/provider.go` | 接入相同观察契约，保留 MLU 自身注入方式 |
| `internal/controller/endpoint_observation.go`、`endpoint_observation_test.go` | 账本占用、真实观察、可用容量、一致性条件和 Warning Event；派生计数不累加 |
| `internal/controller/configuration_controller.go` | 调用观察阶段；监听 Lease；每 15 秒刷新，status 自身变化不导致探测紧循环 |
| `internal/controller/workload_controller.go`、相关测试、`observation_fault_test.go` | 创建新 Remote Pod 前复查；保留既有首次 Ready 回退边界，故障不擅自删除资源 |
| `internal/inventory/reader.go`、`capacity.go`、`capacity_test.go` | 调度/Pool 使用同一观察语义；健康故障不抹掉已保留容量；不显示 Unknown 空闲 |
| `internal/allocation/kubernetes_resolver.go`、`remote.go`、`remote_test.go` | 分配前拒绝 Unknown；原子 Claim 后再实时观察 Busy/故障则回滚；只允许单一独占目标 |
| `internal/policy/local_first.go`、`local_first_test.go` | 删除不受 Allocator 支持的跨 Endpoint Device 聚合；独占不能只选择符合条件的子集；GPU/NPU 本地 0 容量选择 Remote |
| `internal/controller/envtest_test.go` | 真实 API Server 下字段往返、并发抢同 Endpoint 只有一个成功、释放后另一个成功 |
| `api/v1alpha1/acceleratorendpoint_types.go`、deepcopy、Endpoint CRD | Health 允许 Degraded；修复 ObservedAt 指针 deepcopy；生成结果稳定 |
| `examples/remote-real-smoke/` | 全量现场采集、严格 PASS 判定及 8 个反例/正例测试 |
| `config/samples/corex-remote*.yaml`、`mlu-remote*.yaml` | 新镜像 tag、管理端口、数值 workload；MLU Recreate 避免单 Endpoint 后面滚动重叠实例 |
| `runtime/endpoint_observation.h`、`runtime/patches/` | 服务端最小观察实现，以及两个 runtime 仓库可审查、可干净基线应用的源码补丁 |
| `runtime/images/`、`scripts/build-remote-validation.sh`、`build-remote-images.py`、`verify-isolated-remote.py` | 独立目录编译、官方库闭包打包、镜像架构/动态依赖检查和无设备客户端的真实实验 |
| `docs/corex-kubernetes-networking.md`、`remote-observation-and-validation.zh-CN.md` | 纠正之前 NAT 归因、描述状态语义、实验边界和复现命令 |

另外修改了本机 `corex-remote-runtime` 的连接器、服务端和 smoke trace，以及 `mlu-remote-runtime` 的服务端和 BANG smoke 数值标记。补丁与本机实际源码一致，未自动提交这些外部仓库。

## 状态语义

`Reserved` 统计所有未终结 Remote Lease 数量（空 phase、Pending、Reserved、Bound、Releasing）；它是账本，不能声称这些 Lease 都已取得 Claim。`Capacity.Reserved` 则是被独占保留的设备数量。

`ActiveSessions=nil` 是 Unknown；只有远端返回的显式 0 才是“这个 runtime 没有会话”。`Health` 与 session 观察一分钟过期，出错清除旧成功信息。任何 Unknown、Unhealthy、Degraded 或 stale 都不产生新的 Remote 分配。已有 Lease 留在原生命周期，观察失败不自动释放。

零 Lease 却有 session 会触发 `UntrackedSessions` 和 Warning，并阻止新分配。Bound Lease 没有 session 是 `SessionUnconfirmed`，保留占用等待确认。一个 Lease 可以合法创建多个 session/context，不按 session 数量大于 Lease 数量就判错。

## 三条路径的状态机

- **Local**：发现 Node 资源（含 sidecar 正确计算后的已请求量）→ Local 选择 → Lease Pending/Reserved → 注入本地资源 → Pod Bound/Starting/Running → 终结 → Releasing/Released。
- **CoreX Remote**：可信观察 → 选择完整单 Endpoint → 原子 Claim → 实时复查 Healthy/session=0 → Lease Reserved → 稳定 DNS 原样注入、EmptyDir 注入 client 库 → 真正 GPU 执行 → Pod 终结、Lease 释放 → 观察确认 session 归零；不一致显式告警。
- **MLU Remote**：与 CoreX 共用选择/Claim/Lease 生命周期；Provider 保留 MLU 库、shim 和地址变量，执行 BANG kernel；会话与清理由 MLU 自身 worker 表/计数报告。

重复 Reconcile 和重建 Reconciler 从 API 对象重新派生账本，不增加第二次计数。首次 Ready 前保留已有有限 fallback；首次 Ready 后故障只报告运行失败/降级，不伪装成启动失败重新执行。

## 验证记录

| 检查 | 结果 |
| --- | --- |
| `make verify`（fmt/vet/test/build） | PASS |
| `go test -race ./...` | PASS |
| CRD/deepcopy 再生成稳定性 | PASS |
| envtest（真实 etcd/kube-apiserver v1.36.2 ARM64） | PASS；包含并发原子 Claim 与释放后接续 |
| Python smoke verdict 测试 | 8/8 PASS |
| CoreX/MLU runtime server 编译 | PASS；MLU 旧源码中已有 misleading-indentation warnings 保存于 build.log，未把它们写成新增失败 |
| 六个本地 ARM64 镜像构建 | PASS；官方库及传递依赖显式打包，容器内 server ldd 无 not found |
| CoreX 无设备/无 SDK 客户端 → 独立 TCP server | PASS，CPU 数值比对 256 字节，exit=0 |
| MLU 无设备/无 SDK 客户端 → 独立 TCP server | PASS，BANG vector-add 1024 元素 CPU reference，exit=0 |
| CoreX/MLU 无设备容器 + runtime volume 注入 | PASS，MLU 按 Provider 同样注入 shim 的 LD_PRELOAD/LD_LIBRARY_PATH |
| 真实 session 0→1→0、断开清理、Go Provider 读真实服务端 | 两条路径 PASS |
| 停服与清理不确定 | 停服观察不可用；CoreX 测试 worker 中止后 unhealthy + cleanupErrors，不被报告空闲 |
| `git diff --check`、补丁干净基线应用检查、交付镜像/二进制哈希核对 | PASS |

日志与机器可读报告：[本轮证据](evidence/closure-20261118/)。

## 尚未执行或不承诺的边界

1. 本轮没有发布/导入镜像或部署新 K3s 控制器。因此没有声称新版集群 Workload E2E、StatefulSet Pod 重建或 Docker/containerd digest 对齐通过。
2. 独立进程和容器位于同一 ARM 主机；不是物理跨机器/跨节点验收。
3. Session 计数来自目标 runtime，不是整卡其他进程的全局占用查询，也不能证明 session 属于某 Lease；不能原子阻止平台外客户端在观察后接入。
4. MLU 未在真实硬件上故意注入 native cleanup 失败；相关错误路径已做协议解析/Controller 回归，不能记为硬件故障实验 PASS。
5. 旧的无观察能力 Provider（当前 Cricket）会阻止新 Remote 分配；它的旧 Lease 仍可按现有路径释放。这是避免将 Unknown 当空闲的显式行为变化。
6. 新管理接口沿用受信任实验网络边界，未加入 TLS/鉴权。现有 privileged server 清单仍是实验配置。

这些边界没有用默认值、缓存或一次 smoke 成功掩盖。所有复现命令、状态规则、镜像交付细节见 [完整说明](remote-observation-and-validation.zh-CN.md)。
