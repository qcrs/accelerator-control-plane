# 本地平台层与无感切换实现报告

## 1. 文档定位

本文面向平台架构师、Controller 开发人员和资源适配器开发人员，说明本地平台层如何接入，以及用户为什么可以使用同一个 AcceleratorWorkload 接口在本地和远端之间切换。

本文重点是控制面实现，不是 Cricket 网络配置，也不是业务用户的操作手册。相关文档：

- 网络配置：network-two-lan-configuration.zh-CN.md；
- Cricket 运行与验收：cricket-runtime-call-and-validation-report.zh-CN.md；
- Cricket 接入后的操作：cricket-seamless-switching-user-guide.zh-CN.md。

## 2. “无感切换”的准确含义

本项目的无感切换是 S1/S2 语义：

- S1：业务 Pod 创建前选择本地或远端执行位置；
- S2：Pod 第一次 Ready 之前，如果发生可归因于资源或平台运行时的启动失败，删除旧 Pod，释放旧 Lease，再选择另一个执行模式。

它不是以下能力：

- 不是已经运行的进程在两个 GPU 之间热迁移；
- 不是 CUDA Context、显存、stream 或外部副作用的检查点恢复；
- 不是应用程序完全不知道自己被重新创建；
- 不是 Ready 之后的自动跨节点重放。

因此，对用户来说是“一次 Workload 提交，平台自动选择和重试”；对应用来说，失败前的 Pod 可能被删除并重新创建，应用必须能够承受启动重试。

## 3. 控制面分层

整体路径如下：

    AcceleratorWorkload
      -> InventoryReader
      -> PolicyEngine
      -> AcceleratorLease
      -> ExecutionAdapter
           -> Provider
           -> Allocator
      -> RuntimePlan
      -> PodInjector
      -> Kubernetes Pod

各层职责：

| 层 | 职责 | 不负责什么 |
| --- | --- | --- |
| API/CRD | 描述请求、Pool、Endpoint、Lease 和状态 | 不直接调用 CUDA |
| InventoryReader | 读取 Node、Pod、Pool、Endpoint、Lease，归一化候选 | 不做最终所有权仲裁 |
| PolicyEngine | 按模式、优先级和失败排除选择目标 | 不创建 Pod |
| ExecutionAdapter | 绑定 Provider 与 Allocator | 不把具体运行时逻辑放进通用 Controller |
| Provider | 校验配置并生成类型化 RuntimePlan | 不直接持有长期资源状态 |
| Allocator | Reserve、Bind、Release | 不替代 kubelet 或设备插件 |
| PodInjector | 幂等合并环境变量、卷、资源和节点约束 | 不拼接冲突的运行时变量 |
| WorkloadReconciler | 驱动 Lease 状态机、创建/删除 Pod、回退和回收 | 不实现设备隔离数据面 |

## 4. 平台适配层如何注册

当前适配器是 Controller 编译期注册，不是运行时动态插件。入口位于 cmd/manager/main.go：

    executionadapter.NewBinding(
        localprovider.New(),
        &allocation.LocalAllocator{},
    )
    executionadapter.NewBinding(
        cricket.New(),
        remoteAllocator,
    )

Adapter Registry 位于 internal/adapter/registry.go，注册时强制检查：

- Provider 非空；
- Allocator 非空；
- driver 唯一；
- capabilities 至少声明设备类型、执行模式和资源形态；
- Remote allocation mode 只能用于 Remote adapter；
- Provider Registry 与 Adapter Registry 使用同一份注册结果。

这保证了一个 Provider 不会在配置看起来有效、但容量分配器不存在的情况下被选中。

### 4.1 当前两个适配器

本地 Provider 的 driver 是 kubernetes-device-plugin，声明：

- DeviceKind：GPU、NPU；
- Mode：Local；
- ResourceForm：WholeDevice、MIG、Shared、Custom。

Cricket Provider 的 driver 是 cricket，声明：

- DeviceKind：GPU；
- Mode：Remote；
- ResourceForm：WholeDevice；
- RemoteAllocationMode：ExclusiveEndpoint。

新增厂商或运行时时，需要实现 Provider/Allocator、声明 capabilities、补充测试，并在 main.go 注册后重新构建 Controller。当前没有通过 CRD 动态加载任意二进制 Adapter 的机制。

## 5. 本地资源路径

### 5.1 本地资源来源

本地平台层不自己创建 GPU，也不选择物理 UUID。它读取 Kubernetes 已经发布的扩展资源：

    Node.status.allocatable
      + Device Plugin 发布的 resourceName
      + Node labels / selector
      + 当前 Pod 请求
      + 活跃 AcceleratorLease

可以使用：

- NVIDIA Device Plugin 的 nvidia.com/gpu；
- 静态 MIG 资源，例如 nvidia.com/mig-1g.10gb；
- 共享插件发布的扩展资源；
- 其他符合 Kubernetes 扩展资源命名规则的资源。

真实的 Device Plugin、MIG 创建、HAMi 共享和硬件隔离仍由下层平台负责；控制面只负责统一匹配、记账和 Pod 物化。

### 5.2 Inventory 候选过滤

本地候选会依次检查：

1. Pool 的 providerRef、location 和 deviceType；
2. Provider 是否声明该设备类型和 resourceForm；
3. Node 是否 Ready、可调度并匹配 nodeSelector；
4. Pod template 的 nodeSelector、affinity、taints/tolerations；
5. resourceName 的 allocatable 数量；
6. 已运行 Pod 的请求量；
7. 活跃 Lease 的预留量；
8. vendor、model、memory、profile、attributes；
9. 失败历史中的 Node 排除。

数量使用 resource.Quantity。应用容器请求之和、init container 最大请求和 Pod overhead 都会计入容量，避免只计算普通容器而造成逻辑超卖。

### 5.3 Local Provider RuntimePlan

本地 Provider 不注入远端库。它生成的 RuntimePlan 主要包含：

- 目标资源的 requests/limits；
- 选中的 NodeName/hostname 约束；
- mode、provider、pool、lease、resource-name、resource-form 注解。

LocalAllocator 的 Reserve 只记录控制面预留。最终 GPU 分配仍由 scheduler、kubelet 和 Device Plugin 执行；因此本地适配层不会绕过 Kubernetes 的设备分配机制。

## 6. 远端路径在统一接口中的位置

远端 Cricket Provider 使用完全相同的 Workload/Lease 生命周期，但 RuntimePlan 不再添加本地扩展资源，而是添加：

- init container；
- emptyDir runtime volume；
- cricket-client.so 和 libtirpc 挂载；
- REMOTE_GPU_ADDRESS；
- CRICKET_RPCID；
- LD_PRELOAD；
- LD_LIBRARY_PATH。

RemoteAllocator 使用 API Server 中的 coordination.k8s.io/Lease 对 Endpoint 做原子独占。缓存只用于候选选择，最终 claim 是并发冲突的权威判断。

这就是平台适配层的价值：通用控制器只知道 Provider、Allocator 和 RuntimePlan，不需要知道 CUDA RPC、NVIDIA Device Plugin 或具体端口。

## 7. Lease 状态机

Workload 使用 namespaced AcceleratorLease 保存持久化选择和资源状态：

    Pending
      -> Reserved
      -> Bound
      -> Running
      -> Releasing
      -> Released

关键对象关系：

    AcceleratorWorkload
      owns AcceleratorLease
        binds generated Pod
          references local Node or remote Endpoint

Lease 保存：

- driver；
- Provider/Pool 引用；
- Local 或 Remote mode；
- 不可变目标快照；
- TTL 和 ExpiresAt；
- Remote binding 中的地址、runtime 和参数；
- PodRef；
- allocation phase；
- finalizer。

Controller 重启后从 API 对象恢复，不依赖内存中的选址或重试列表。

## 8. Auto、LocalOnly 和 RemoteOnly

### 8.1 LocalOnly

只考虑本地 Pool。没有可用本地资源时保持 Pending/Unschedulable，不尝试 Cricket。

### 8.2 RemoteOnly

只考虑远端 Pool。适合当前没有本地 GPU、需要明确验证远端数据面的场景。

### 8.3 Auto

默认策略是 Local -> Remote。也可以显式指定：

    placement:
      mode: Auto
      order: [Local, Remote]

当本地候选存在且预留成功时，业务 Pod 只带本地资源请求，不带远端 shim。

当远端候选被选中时，业务 Pod 只带 Cricket runtime，不申请本地受管 GPU 资源。

这种模式隔离避免同一个 Pod 同时声明本地 GPU 和远端 CUDA shim。

## 9. S2 启动失败回退

Fallback 只在以下条件同时满足时启用：

- placement.mode 为 Auto；
- 当前 Workload 同时存在本地和远端可选模式；
- Pod 从未出现过 PodReady=True；
- 失败原因属于平台可归因的启动失败；
- 未超过 maxAttempts 和 startupTimeout。

默认值：

    maxAttempts: 2
    startupTimeout: 5m
    backoff: 5s

可回退原因包括：

- scheduler Unschedulable；
- 远端 Endpoint 在 Ready 前失效；
- Accelerator 管理的 runtime init container 拉取、创建、运行或退出失败；
- 用户显式配置的 startupTimeout 到期；
- Pod 创建前的可重试 Reserve 或 RuntimePlan 冲突。

不会立即触发跨模式回退的原因包括：

- 应用镜像错误；
- 用户自己的 init container 失败；
- 应用进程退出；
- Pod 第一次 Ready 之后的进程故障；
- 永久的 Pod template 冲突或不支持的 RuntimePlan。

### 9.1 回退清理顺序

控制器先写入失败历史，再执行破坏性清理：

    写入失败历史
      -> 删除旧 Pod
      -> 等待 Pod NotFound
      -> Release allocation Lease
      -> 删除 Endpoint claim
      -> 记录 RequeueAt
      -> 选择另一模式

如果 Pod 创建请求结果未知，控制器不会立即释放资源，而是保留确定性 Pod 名称和预留，避免“Pod 实际已创建但资源已释放”的并发错误。

### 9.2 Ready 边界

status.fallback.readyObserved 是单调值。一旦 Pod 曾经 Ready：

- 不再执行 S2 自动回退；
- 后续进程退出按应用结果处理；
- Lease 正常释放；
- Workload 进入 Succeeded 或 Failed。

这保证平台不会把普通应用失败重复运行到另一张 GPU 上。

## 10. Pod 注入的无感边界

PodInjector 对 RuntimePlan 做幂等合并：

- 相同 volume、init container、env、mount、资源和注解重复 reconcile 不会重复追加；
- 同名但内容不同的配置被判定为冲突；
- 本地模式添加扩展资源和节点约束；
- 远端模式添加运行时卷、init container 和环境变量；
- 用户已经声明不同 LD_PRELOAD 或 LD_LIBRARY_PATH 时拒绝静默覆盖。

用户只需要提交统一的 podTemplate。平台根据最终选择的 mode 物化不同 Pod。应用不需要读取平台 API 或自行拼接远端地址。

## 11. 状态、事件与审计

Workload 和 Lease 通过 Conditions、Phase 和 status.fallback.history 记录状态。事件覆盖：

- Selected；
- Reserved；
- PodCreated；
- Bound；
- Ready；
- StartupFailed；
- FallbackScheduled；
- FallbackExhausted；
- Released；
- ConfigurationReady；
- ConfigurationInvalid。

审计时应同时查看：

    kubectl get acceleratorworkload <name> -o yaml
    kubectl get acceleratorlease <name> -o yaml
    kubectl describe pod <generated-pod>
    kubectl get events --sort-by=.lastTimestamp

## 12. 真实平台接入边界

已经完成的是通用控制面语义：

- Provider/Allocator 配对；
- 本地扩展资源发现；
- 精确容量记账；
- 远端 Endpoint 独占；
- RuntimePlan 注入；
- 有界启动回退；
- Lease 恢复和释放。

仍需独立验收或实现的是：

- NVIDIA Device Plugin 的真实调度行为；
- MIG 创建、重配置和硬件隔离；
- HAMi 或其他共享运行时的实际限制；
- NPU 厂商 Device Plugin 和 SDK；
- Endpoint 主动网络探针；
- Cricket TLS/认证或替代 runtime；
- Prometheus 指标、配额和多租户准入；
- Ready 之后的检查点恢复和热迁移。

## 13. 验收建议

- [ ] LocalOnly 只选择本地扩展资源。
- [ ] RemoteOnly 只选择 Cricket Endpoint。
- [ ] Auto 在本地有容量时优先本地。
- [ ] 本地候选不可调度时可以切远端。
- [ ] 远端 init runtime 失败时可以在 Ready 前切本地。
- [ ] 应用镜像失败不会立即跨模式重放。
- [ ] Pod Ready 后不再自动回退。
- [ ] Controller 重启不重置失败次数和 deadline。
- [ ] 两个 Workload 并发竞争同一远端 Endpoint 时只有一个 claim 成功。
- [ ] Workload、Pod 或 Lease 删除后本地预留和远端 claim 都能回收。
