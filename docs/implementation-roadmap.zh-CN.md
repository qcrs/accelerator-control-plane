# S1/S2 多形态加速器控制面技术路线与实现状态

## 1. 结论与本轮边界

基于 `D:\vGPU\docs` 的总体路线，当前仓库已从“接口与主流程原型”推进为可构建、可部署、可通过真实 Kubernetes API Server 仿真的 S1/S2 控制面实现。

本轮交付边界是：

- 实现 S1 启动前本地/远端自动选择；
- 实现 S2 首次 Ready 前的有界回退；
- 本地控制面覆盖 Kubernetes Device Plugin 发布的整卡、MIG、共享和自定义扩展资源；
- 远端执行复用静态部署的 Cricket server，内置 Cricket Adapter 仍只支持整卡 GPU；
- 不实现 S3 检查点恢复、S4 热迁移，也不负责创建/重配 MIG、实现 HAMi 虚拟化、选择物理设备 UUID 或落实硬件隔离；
- 本轮已验证真实 Windows/WSL RTX 5060 Ti 的 Cricket RPC/CUDA 数据面，以及经
  `AcceleratorWorkload` 创建的 RemoteOnly/Auto 容器；真实本地 Device Plugin、MIG、共享插件
  和 NPU 数据面仍未在本环境验收；
- 真实 Cricket 链路已纳入验收，但生产环境仍需补充生命周期、网络安全、健康探针和 CUDA/框架
  兼容矩阵。

因此，“代码完成”不等于“生产验收完成”。真实硬件兼容性、数据面性能、网络安全和故障注入仍属于后续准入工作。

## 2. 当前架构

```text
AcceleratorWorkload
  -> InventoryReader
       -> Local: Node allocatable - Pod request - active Lease
       -> Remote: Endpoint health - active Lease
  -> LocalFirst Policy
  -> AcceleratorLease（不可变目标与资源快照）
  -> ExecutionAdapter（Provider + Allocator）
       -> Local: Device Plugin extended resource
       -> Remote: coordination Lease CAS + immutable Endpoint snapshot
  -> Provider RuntimePlan
       -> Local: resource request + selected Node affinity
       -> Cricket: init runtime + LD_PRELOAD + Endpoint env
  -> deterministic Pod
  -> Bound / Running / Released
```

核心 API 分工如下：

| API | 作用 | 作用域 |
| --- | --- | --- |
| `AcceleratorProvider` | Provider 驱动和客户端运行时配置 | 集群 |
| `AcceleratorPool` | 本地或远端资源池、优先级和容量状态 | 集群 |
| `AcceleratorEndpoint` | 静态远端 server、运行时版本和可见设备 | 集群 |
| `AcceleratorWorkload` | 用户请求、放置和回退策略 | 命名空间 |
| `AcceleratorLease` | 不可变选择、预留、绑定和回收记录 | 命名空间 |
| `coordination.k8s.io/Lease` | 远端 Endpoint 的原子独占权 | 控制器命名空间 |

## 3. 已落地能力与验证

| 能力 | 当前实现 | 已有验证 | 尚未完成的真实环境验证 |
| --- | --- | --- | --- |
| 本地扩展资源选择 | `WholeDevice`/`MIG`/`Shared`/`Custom` 统一发现、属性过滤、Lease 快照、扩展资源与 Node 约束注入 | 单元测试、fake client 生命周期 | NVIDIA Device Plugin、MIG、HAMi/其他共享插件 + 真实调度 |
| 跨 Pool 本地记账 | 相同 `resourceName` 的 Pool 共享预留视图，避免逻辑超卖 | 单元测试 | 高并发真实集群压测 |
| 精确本地容量 | 使用 `resource.Quantity`，计入应用容器之和、init 容器最大值和 Pod overhead；Pool 删除后仍按 Lease 快照记账 | 单元测试 | 目标调度器和 Device Plugin 的实际计量语义 |
| 远端整卡选择 | Cricket 只接受一个独占 Endpoint | 单元测试、fake client、kind + 真实 Cricket server 平台 E2E | 多副本、高延迟和故障注入 |
| 设备可见性 | 请求数必须等于静态 server 暴露的健康设备数 | 策略/分配器双层测试、远端容器 `cudaGetDeviceCount=1` 与多 kernel 校验 | 多 GPU/不同架构兼容矩阵 |
| 原子远端预留 | 哈希命名 coordination Lease，Create 作为 CAS，UID 作为 fencing token | fake client + Kubernetes 1.36.2 envtest | 多副本、高延迟、故障注入 |
| 不可变绑定 | 地址、runtime 和参数复制到 allocation Lease status | 单元测试 | server 重启/改址演练 |
| 版本兼容 | `clientRuntime.runtimeVersion` 必须等于 Endpoint `runtime.version` | Provider 单元测试、当前 pinned Cricket 版本真实 E2E | CUDA/框架兼容矩阵 |
| Pod 创建幂等 | 确定性命名；超时/传输错误不释放，永久拒绝才释放 | 分类单元测试 | API Server 丢响应演练 |
| S2 回退 | 失败历史、次数预算、绝对 backoff、反向模式偏好、失败目标排除 | 本地失败切远端的 fake-client 全流程测试 | 真实调度和镜像故障演练 |
| 配置健康 | Provider/Pool `Ready`、Endpoint `Configured`，拒绝代次过期状态 | 单元测试 | 外部网络探针尚未实现 |
| 容量状态 | Pool 从 Node/Pod/Endpoint/Lease 实时汇总 | 单元测试 | Prometheus 指标尚未实现 |
| 部署基础 | Dockerfile、RBAC、leader election、PDB、双副本 Deployment、Kustomize | Kustomize build | 目标集群部署 |
| API 清单 | controller-gen 生成完整 CRD OpenAPI 和 DeepCopy | envtest 安装、默认值和 CAS 测试 | 升级/回滚演练 |

## 4. 必须保持的正确性不变量

### 4.1 远端资源不能超卖

库存缓存只用于候选选择，不作为最终所有权依据。最终权威操作是创建 Endpoint 对应的 coordination Lease：

1. 名称由 Endpoint 名称稳定哈希得到；
2. holder 是 `AcceleratorLease.UID`，不能使用可复用的普通名称；
3. 相同 holder 重试幂等；
4. 不同存活 holder 返回冲突；
5. owner 不存在、终态或 UID 不一致时才回收孤儿 claim；
6. 删除 claim 时校验 holder，并使用 UID/resourceVersion precondition。

这使控制器缓存陈旧、leader 切换和重复 reconcile 不会导致两个 Workload 同时拿到同一个 Cricket server。

### 4.2 不能释放仍可能被 Pod 使用的资源

清理顺序固定为：

```text
记录失败
  -> 删除旧 Pod
  -> 等待 Pod NotFound
  -> 释放 allocation Lease / Endpoint claim
  -> 清空 active allocation
  -> 重新选择
```

Pod Create 超时属于“提交结果未知”，不能立即释放。控制器保留预留并用确定性 Pod 名称重试；如果预留最终过期，必须先删除可能已创建的 Pod，再释放资源。

### 4.3 静态 Cricket 不能暴露未分配 GPU

当前 Cricket server 的 `CUDA_VISIBLE_DEVICES` 在进程启动时固定，客户端不能按 Lease 隐藏其中一部分设备。因此一个 Endpoint 的健康设备数必须与 Workload 请求数完全相等。若需要独立分配多张卡，应启动多个受限 server 并声明多个 Endpoint；这不是 MIG 或设备切分。

### 4.4 S2 不能把普通应用错误当作资源故障

立即跨目标回退只覆盖：

- scheduler `Unschedulable`；
- 选中远端 Endpoint 在 Ready 前失效；
- 控制面注入的 runtime init container 拉取、创建、运行或退出失败；
- 用户显式配置的 `startupTimeout` 到期。

应用镜像错误、用户 init container 失败和应用进程退出不会立即换 GPU 重放，以免无效重试和重复副作用。注入器会把其新增的 init container 名称写入 Pod annotation，供控制器准确分类。

## 5. 分阶段路线

### M0：工程和 API 基线，已完成

- Go 模块依赖锁定和 `go.sum`；
- 标准 CRD/DeepCopy 生成；
- 非 root 镜像；
- RBAC、leader election、健康探针、PDB 和 Kustomize；
- 单元测试与 envtest 基础设施。

### M1：S1 多形态本地资源/远端整卡 GPU 控制面，代码完成，待硬件验收

- 本地/远端统一候选；
- 本地优先与 RemoteOnly/LocalOnly；
- allocation Lease 状态机；
- 本地整卡、MIG、共享和自定义扩展资源的发现、匹配、记账、快照与注入；
- 可按 Device Plugin 驱动注册不同的本地 ExecutionAdapter；
- Cricket runtime 注入和不可变 Endpoint binding；
- 远端 Endpoint 原子独占；
- 终态和删除回收。

建议真实环境验收工期为 1～2 周，取决于 Cricket/CUDA/框架镜像准备情况。

### M2：S2 启动失败回退，代码完成，待故障演练

- durable failure history；
- retry budget 和 backoff；
- 失败 Node/Endpoint 排除；
- Local -> Remote 和 Remote -> Local 策略；
- Ready 后禁止自动重放；
- runtime setup 与应用错误分类。

建议真实故障矩阵和稳定性验收工期为 1～2 周。

### M3：生产化增强，建议 2～4 周

优先级从高到低：

1. 实现独立 Endpoint 网络探针，维护 `Ready`、`lastProbeTime` 和探针时效；
2. 给 Cricket 链路增加网络隔离；若要跨不可信网络，必须引入 TLS/身份认证能力或更换 runtime；
3. 增加 Prometheus 指标、告警、SLO 和容量趋势；
4. 增加 namespace quota、并发限额和准入策略；
5. 执行多副本 leader 切换、API 超时、Pod 删除阻塞、claim 孤儿回收测试；
6. 建立 CUDA、驱动、框架、算子和 Cricket commit 兼容矩阵；
7. 形成升级、回滚、备份和事故处理手册。

### M4：真实切分/隔离数据面与 NPU 接入，4～6 周起，当前明确延期

本轮已经完成控制面通用基础：资源形态、profile、隔离级别、属性、显存元数据、精确 Quantity 记账、不可变 Lease 目标，以及任意扩展资源名的 requests/limits 注入。它证明的是 Kubernetes 控制面语义，不等于已经实现 MIG 或共享 GPU 数据面。

生产接入至少还需要：

- 在目标集群部署并验证 NVIDIA MIG、DRA/HAMi 或目标 NPU Device Plugin；
- 明确由谁创建/重配 MIG、分配物理 UUID、执行显存/算力限制并提供隔离保证；
- 校准不同插件的资源单位、profile、init container、Pod overhead、拓扑和调度语义；
- 对动态重配置补充碎片整理、拓扑约束、配额和故障恢复状态机；
- 若远端也要切分，提供按 session 隔离设备可见性的 runtime/Agent，并将 Endpoint 级 CAS 下沉为设备实例或 slice 级所有权；静态 Cricket Endpoint 不满足该要求；
- 为不同 NPU 厂商 SDK 单独实现 Adapter、镜像和兼容矩阵。

预计 4～6 周只是单一厂商、有限 profile 的真实数据面起点，多厂商和生产隔离通常需要更长周期。

S4 运行中无损热迁移同样不在当前计划内；它需要迁移显存、CUDA context、stream 和外部副作用一致性，不能由控制面重建 Pod 来等价替代。

## 6. 后续验收矩阵

### 控制面

- 100 个并发 Workload 不发生本地逻辑超卖；
- 同一 Endpoint 并发抢占只有一个 claim 成功；
- leader 在 select/reserve/create/bind/release 各阶段切换后可恢复；
- API Create 响应丢失不会提前释放；
- Workload/Lease/Pod 直接删除均不泄漏 claim；
- Pool status 与实际 Node/Pod/Lease 一致。
- 整卡、MIG 和共享资源使用各自扩展资源名，不发生跨资源名误扣减；
- 大/小数量单位、init container、Pod overhead 和 Pool 删除场景保持精确记账；

### S2

- 本地不可调度后切远端；
- 远端 Endpoint Ready 失效后切本地；
- runtime init 镜像失败消耗一次预算；
- 应用镜像失败不立即跨目标重放；
- 到达 Ready 后进程失败只终止，不回退；
- manager 重启不重置次数、deadline 或排除列表。

### 真实数据面

- 本地整卡、静态 MIG 和目标共享 Device Plugin 的资源分配与隔离；
- 动态链接 CUDA 样例；
- `cudaGetDeviceCount` 与 Lease 数量一致；
- 目标 PyTorch/训练框架；
- CUDA/Driver/Cricket 版本矩阵；
- 网络抖动、server 重启、rpcbind 故障；
- 本地与远端吞吐、延迟和稳定性基线。

## 7. 当前风险结论

- **控制面一致性**：已具备较完整的代码级和 envtest 保障，可进入真实集群验证。
- **远端数据面安全**：Cricket 原生没有 TLS/认证，当前只能用于可信隔离网络，不能直接按生产多租户标准验收。
- **远端健康**：目前允许无 `Ready` 的静态 Endpoint，适合 PoC；生产必须补主动探针和状态时效。
- **兼容性**：runtime version 已做精确门禁，但 CUDA/框架 ABI 仍需实机矩阵。
- **本地多形态资源**：控制面匹配、记账和注入已实现；MIG/HAMi/其他 Device Plugin 的真实隔离、UUID 和硬件行为尚未验收。
- **高级连续性**：当前只有首次 Ready 前回退，不包含检查点恢复或运行中迁移。

建议下一步先部署一套最小真实集群，完成 M1/M2 的硬件与故障矩阵，再决定是否进入 M3。真实切分/NPU 数据面和 S4 应保持独立立项，避免把尚未验证的隔离能力混入当前可验证的 S1/S2 控制面边界。
