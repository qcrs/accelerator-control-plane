# 本地/远端异构加速资源控制面技术设计

> 文档类型：目标态技术设计（Design Specification）  
> 文档状态：设计草案  
> 适用阶段：方案评审、任务拆分、接口评审、测试设计和部署规划  
> 特别说明：本文只定义系统应如何设计、需要完成哪些工作以及如何验收，不记录代码、部署或测试的实际完成状态。不得依据本文推断项目实施进度。

> 当前实现提示：本文保留目标态设计和阶段边界；仓库已经额外完成 Cricket
> Provider/Allocator、真实 Windows/WSL CUDA 数据面和平台容器 E2E。实施状态、复现命令
> 和已知限制以 [`docs/cricket-integration.md`](cricket-integration.md) 及
> [`examples/cricket-e2e/complex-demo/README.zh-CN.md`](../examples/cricket-e2e/complex-demo/README.zh-CN.md)
> 为准。

## 1. 文档目的

本文设计一套运行在 Kubernetes 上的异构加速资源控制面，使业务工作负载能够在启动前选择本地或远端加速资源，并在第一次 Ready 之前对有限类型的启动故障执行有界回退。

设计重点包括：

- 使用统一资源模型描述本地整卡、MIG、软件共享份额和其他 Device Plugin 扩展资源；
- 使用 `ExecutionAdapter` 隔离具体设备插件、虚拟化方案和远端运行时；
- 使用 Kubernetes API 对选择、预留、绑定、回收和故障历史进行持久化；
- 保证一个业务 Pod 在任一时刻只使用一种执行模式；
- 将缓存选址与最终资源所有权分开，避免并发超卖；
- 提供不依赖真实 GPU 的控制面测试路径，以及依赖真实硬件的数据面验收路径；
- 为 HAMi、MIG、其他 GPU Device Plugin、本地 NPU 和未来远端运行时预留清晰而受约束的扩展点。

本文面向架构师、Kubernetes Controller 开发人员、设备插件集成人员、测试人员和平台运维人员。

## 2. 背景与问题定义

传统 Kubernetes GPU Pod 通常直接声明某个扩展资源，例如 `nvidia.com/gpu`。这种方式适合单一设备类型和单一执行路径，但难以统一处理以下问题：

1. 本地资源可能是整卡、静态 MIG 实例、HAMi 软件份额或厂商自定义资源，容量单位和隔离语义并不相同；
2. 本地资源不足时，平台可能希望在 Pod 创建前选择其他执行底座；
3. 具体运行时会要求不同的 init container、挂载、环境变量、认证材料或调度约束；
4. Controller 缓存可能过期，多副本并发选择同一资源时需要最终所有权仲裁；
5. Pod 创建、绑定和删除都可能出现“结果未知”，不能简单释放资源；
6. 应用错误、设备错误和运行时准备错误必须区分，否则自动重试可能重复业务副作用；
7. Controller 重启后必须从 Kubernetes 对象恢复，而不能依赖内存状态。

因此，本系统不应只是一个修改 Pod 的 webhook，而应是一套包含资源抽象、策略选择、持久化租约、运行时物化、状态机和回收机制的控制面。

## 3. 设计目标

### 3.1 功能目标

系统需要支持：

- `Auto`、`LocalOnly`、`RemoteOnly` 三种放置模式；
- 默认 `Local -> Remote` 的选择顺序，以及可配置顺序；
- S1：业务 Pod 创建前完成本地/远端选择；
- S2：业务 Pod 第一次 Ready 前，在满足故障分类和次数预算时回退到另一执行模式；
- 本地整卡、MIG、共享份额和自定义扩展资源的统一候选模型；
- Provider 与 Allocator 成对注册的执行适配器；
- 不可变的选择目标、资源单位和运行时绑定快照；
- 幂等的 Reserve、Bind、Release 和 Pod 物化；
- 冲突可检测的 Pod 注入；
- Workload、Lease、Pod 和资源所有权之间的可靠回收；
- Kubernetes Events、Conditions 和容量状态；
- fake client、单元测试、envtest、CPU 集群和真实 GPU 集群的分层验证。

### 3.2 非功能目标

系统应满足：

- **一致性**：不会因为缓存过期或重复 reconcile 将独占资源交给两个 Workload；
- **可恢复性**：Controller 重启后能够继续选择、预留、创建、绑定、回退或回收；
- **可扩展性**：增加新 Adapter 时不修改通用 Workload 状态机和放置策略；
- **可审计性**：关键决策、失败原因和资源绑定都能从 API 对象或 Event 追溯；
- **最小权限**：Controller 仅获得管理目标 CRD、Pod、Event 和协调 Lease 所需权限；
- **模式隔离**：Local Pod 不带远端 shim，Remote Pod 不申请受管本地设备资源；
- **确定性**：相同输入、相同库存快照和相同策略应产生稳定排序结果；
- **可测试性**：核心控制面逻辑不依赖 CUDA、NVML、厂商 SDK 或真实网络服务。

## 4. 非目标与边界

以下能力不属于本设计的基础交付范围：

- Pod Ready 之后的热迁移；
- CUDA Context、显存、stream 或事件的检查点与恢复；
- 应用进程透明续跑；
- 在通用 Controller 内实现 CUDA/NVML/RPC 转发；
- 在控制面内创建物理 GPU、选择物理 UUID 或绕过 kubelet 分配设备；
- 由控制面实现 HAMi 的显存/算力虚拟化；
- 由控制面动态创建或重配置 MIG 实例；
- 将不同资源形态粗略等价为同一种“GPU 数量”；
- 未经专用 Adapter 验证的本地 NPU 或远端 NPU 数据面；
- 将远端运行时的固定文件名、环境变量、协议端口或 RPC 标识放入通用层。

在本文原始设计基线中，远端执行只要求保留 Provider/Allocator/RuntimePlan 接口和一致性边界；
当前实现已经通过具体 Cricket Adapter 接入真实远端运行时。网络安全、服务生命周期、健康探针
和生产化发现仍属于交付加固工作，不应与“适配层是否接入”混为一谈。

## 5. 术语与连续性等级

| 术语 | 定义 |
| --- | --- |
| Workload | 用户提交的加速工作负载声明，不等于实际 Pod |
| Candidate | InventoryReader 归一化后的一个可选择资源单元 |
| Provider | 校验执行底座配置并生成 RuntimePlan 的组件 |
| Allocator | 对资源执行 Reserve、Bind、Release 的组件 |
| ExecutionAdapter | 同一 driver 下不可拆分的 Provider/Allocator 组合 |
| RuntimePlan | Provider 输出的类型化 Pod 物化计划 |
| Allocation Lease | 记录选择目标、预留、绑定和回收状态的业务租约 |
| Coordination Lease | 用于并发所有权仲裁的 Kubernetes `coordination.k8s.io/Lease` |
| S1 | Pod 创建前选择执行位置 |
| S2 | Pod 第一次 Ready 前删除旧 Pod、释放旧资源并重新选择 |
| S3 | 基于检查点重新创建工作负载，不保证进程透明 |
| S4 | 运行中上下文与状态迁移 |

本系统基础设计只覆盖 S1 和受约束的 S2。

## 6. 总体架构

```text
┌──────────────────────────┐
│ AcceleratorWorkload      │
│ request + placement      │
└─────────────┬────────────┘
              │ reconcile
              v
┌──────────────────────────┐
│ InventoryReader          │
│ Node/Pod/Pool/Endpoint   │
│ -> normalized Candidate  │
└─────────────┬────────────┘
              │ compatible candidates
              v
┌──────────────────────────┐
│ PolicyEngine             │
│ filter/order/select      │
└─────────────┬────────────┘
              │ immutable decision
              v
┌──────────────────────────┐
│ AcceleratorLease         │
│ target/resource snapshot │
└─────────────┬────────────┘
              │ driver lookup
              v
┌──────────────────────────────────────┐
│ ExecutionAdapter                    │
│  Provider               Allocator   │
│  validate/plan          reserve/bind│
│                         /release    │
└─────────────┬────────────────────────┘
              │ RuntimePlan
              v
┌──────────────────────────┐
│ Mode Validator           │
│ PodInjector              │
└─────────────┬────────────┘
              │ deterministic Pod
              v
┌──────────────────────────┐
│ kube-apiserver/scheduler │
│ kubelet/Device Plugin    │
└──────────────────────────┘
```

### 6.1 控制流

标准控制流应为：

1. 读取并校验 Workload；
2. 根据 Provider、Pool、Node、Pod、Endpoint 和 Lease 构建候选；
3. 进行设备类型、形态、profile、显存、标签、属性和健康过滤；
4. PolicyEngine 根据模式、顺序、优先级和失败排除选择目标；
5. 创建包含不可变目标快照的 AcceleratorLease；
6. 通过 Provider driver 获取唯一 ExecutionAdapter；
7. Allocator 预留资源；
8. Provider 根据绑定后的 Lease 生成 RuntimePlan；
9. 通用校验器验证 Local/Remote 模式边界；
10. PodInjector 将 RuntimePlan 幂等应用到 PodTemplate；
11. 使用确定性名称创建 Pod；
12. Allocator 将租约与 Pod 绑定；
13. 观察 Pod Ready、终态、删除或启动失败；
14. 根据结果进入 Running、Release 或 S2 清理与重新选择。

### 6.2 数据面边界

控制面只决定“使用哪类资源、在哪个目标运行、如何物化 Pod”。实际设备分配和调用路径由下层负责：

- 本地整卡/MIG：kube-scheduler、kubelet 和 Device Plugin；
- 本地共享：HAMi 或其他共享 Device Plugin/调度组件；
- 远端执行：具体远端 runtime client/server；
- NPU：厂商 Device Plugin、DRA driver 或专用 runtime。

## 7. API 资源设计

建议使用五类 CRD。

### 7.1 AcceleratorProvider

职责：声明执行底座 driver、支持能力和运行时配置。

建议字段：

```yaml
spec:
  driver: kubernetes-device-plugin
  deviceKinds: [GPU]
  capabilities:
    modes: [Local]
    resourceForms: [WholeDevice, MIG, Shared, Custom]
    allocationModes: []
    tls: false
    auth: false
  clientRuntime: {}       # 仅远端 Provider 按需使用
  parameters: {}          # 结构化能力不足时的受控扩展参数
```

设计要求：

- `driver` 必须非空且在 Adapter Registry 中唯一注册；
- Provider 声明的能力必须与 Adapter 能力一致；
- `parameters` 不能代替核心类型化字段；
- Secret 只能通过引用传递，不得复制到 status、Event 或普通参数；
- status 应通过 `Ready` Condition 表达静态配置是否有效。

### 7.2 AcceleratorPool

职责：描述一组同类本地资源或远端 Endpoint，并提供策略优先级。

本地示例：

```yaml
spec:
  providerRef:
    name: local-device-plugin
  location: Local
  deviceType: GPU
  priority: 100
  local:
    resourceName: nvidia.com/mig-1g.10gb
    resourceForm: MIG
    profile: 1g.10gb
    isolation: hardware-partition
    vendor: nvidia
    model: A100
    memory: 10Gi
    nodeSelector: {}
    attributes: {}
```

远端示例：

```yaml
spec:
  providerRef:
    name: remote-runtime-a
  location: Remote
  deviceType: GPU
  priority: 10
  remote:
    allocationMode: ExclusiveEndpoint
    endpointSelector: {}
```

设计要求：

- Local 和 Remote 配置必须二选一；
- 本地 `resourceName` 必须从配置读取，通用代码禁止写死；
- Pool status 可发布 total、available、reserved 和 observedGeneration；
- Pool 只是逻辑视图，多个 Pool 引用相同 Node/resourceName 时不得复制物理容量；
- 不同 resourceName 的数量不可直接相加为一种设备容量。

### 7.3 AcceleratorEndpoint

职责：声明一个静态远端运行时目标或未来动态 session 的入口。

建议字段：

```yaml
spec:
  poolRef:
    name: remote-pool-a
  address: 10.0.0.25
  runtime:
    driver: runtime-a
    version: v1.2.3
    apiVersion: v1
  devices:
    - id: gpu-0
      vendor: nvidia
      model: A100
      memory: 40Gi
      resourceForm: WholeDevice
      health: Healthy
      attributes: {}
  parameters: {}
```

设计要求：

- address、runtime 和 devices 需要在预留时复制到不可变绑定；
- `Configured` 表达静态结构合法性，`Ready` 留给主动健康观察器；
- 健康条件必须带 observedGeneration，旧代次状态不能用于新选择；
- Endpoint 的设备级分配只有在 Adapter 明确声明隔离能力时才允许。

### 7.4 AcceleratorWorkload

职责：描述业务 Pod、加速器需求、放置策略和 S2 策略。

```yaml
spec:
  accelerator:
    deviceType: GPU
    vendor: nvidia
    count: 1
    memory: 10Gi
    resourceForm: MIG
    profile: 1g.10gb
    isolation: hardware-partition
    models: [A100]
    matchLabels: {}
    matchAttributes: {}
    containerName: worker
  placement:
    mode: Auto
    order: [Local, Remote]
    reservationTTL: 5m
    fallback:
      enabled: true
      maxAttempts: 2
      startupTimeout: 5m
      backoff: 5s
  podTemplate:
    spec:
      containers:
        - name: worker
          image: example/workload:tag
```

status 至少需要记录：

- phase；
- 当前决策、Provider、Pool、目标和执行模式；
- allocationRef 和 podRef；
- observedGeneration；
- 是否曾 Ready；
- S2 失败次数、历史、排除目标和 requeueAt；
- Conditions 和人类可读原因。

### 7.5 AcceleratorLease

职责：作为资源选择、预留、绑定和回收的持久化事实记录。

Lease spec 应不可变，并至少包含：

- owner Workload UID、名称、命名空间和 generation；
- 原始加速器请求；
- 不可变的 Adapter driver 快照、ProviderRef、PoolRef 和执行模式；driver 快照用于在 Provider/Pool 已删除时仍能路由到原 Allocator 完成 Release；
- 本地目标的 Node、resourceName、数量、形态、profile、隔离和属性；
- 远端目标的 Endpoint 标识；
- reservation TTL。

Lease status 应包含：

- `Pending/Reserved/Bound/Releasing/Released/Failed/Expired`；
- reservation ID、过期时间和 PodRef；
- 远端不可变 binding snapshot；
- RuntimeReady、Bound、Released 等 Conditions；
- 最近错误和 observedGeneration。

## 8. ExecutionAdapter 设计

### 8.1 设计动机

Provider 负责“如何把已选择资源变成 Pod 运行计划”，Allocator 负责“谁拥有资源”。二者若独立按名称查找，可能错误地把一个 Provider 与另一个底座的 Allocator 组合。因此二者必须在同一个 driver 注册边界内成对出现。

### 8.2 接口建议

```go
type ExecutionAdapter interface {
    Provider() Provider
    Allocator() Allocator
}

type Provider interface {
    Driver() string
    Capabilities() CapabilitySet
    Validate(provider *AcceleratorProvider, pool *AcceleratorPool) error
    BuildRuntimePlan(ctx context.Context, provider, pool, lease) (RuntimePlan, error)
}

type Allocator interface {
    Reserve(ctx context.Context, request ReservationRequest) (Reservation, error)
    Bind(ctx context.Context, request ReservationRequest, pod ObjectReference) error
    Release(ctx context.Context, request ReservationRequest) error
}
```

### 8.3 Registry 规则

Registry 必须：

- 拒绝空 driver；
- 拒绝 nil Provider 或 nil Allocator；
- 拒绝重复 driver；
- 拒绝空能力集合和重复能力；
- 对未知 driver 返回明确 `Unsupported`；
- 为配置校验 Controller 和 Workload Controller 提供同一 Provider 视图；
- 不允许 Controller 绕过 Registry 直接构造某个具体 Provider。

### 8.4 错误分类

统一错误至少分为：

| 错误类型 | 语义 | 默认处理 |
| --- | --- | --- |
| Invalid | 用户或 Provider 配置不合法 | 终止当前 Workload，等待配置变更 |
| Unsupported | driver、模式或能力未支持 | 终止并报告明确原因 |
| Unschedulable | 当前没有满足条件的容量 | 等待库存变化，或按 S2 策略处理 |
| Conflict | 所有权、缓存或注入发生冲突 | 幂等重试或按 S2 处理 |
| Transient | API、网络或暂时依赖故障 | 保留资源事实，退避重试 |
| Permanent | 无法通过重试修复 | 失败并安全回收 |

错误分类必须由最接近事实的组件产生，Controller 不应通过字符串匹配猜测。

## 9. InventoryReader 与候选模型

### 9.1 Candidate 字段

统一 Candidate 至少需要：

- ID、Provider、Pool、优先级；
- Local/Remote 模式；
- GPU/NPU 设备类型；
- Node 或 Endpoint；
- resourceName、resourceForm、profile、isolation；
- available quantity；
- vendor、model、memory；
- labels、attributes；
- health/readiness；
- 是否独占；
- 设备列表和运行时信息（远端）。

Candidate 是控制面选择单元，不应携带可直接执行厂商 SDK 的对象。

### 9.2 本地库存

本地候选应来自：

- Node Allocatable；
- 非终态 Pod 的 requests；
- 未被对应 Pod 消化的活跃 AcceleratorLease；
- Pool 的 NodeSelector、资源形态和属性配置；
- Node Ready、taint/toleration 和 Pod 硬调度约束。

本地可用量计算：

```text
available(node, resourceName)
  = allocatable(node, resourceName)
  - requestedByNonTerminalPods(node, resourceName)
  - activeUnconsumedLeaseReservations(node, resourceName)
```

Pod request 应按 Kubernetes 调度语义计算：

```text
podRequest
  = max(sum(regularContainers), max(initContainers))
  + podOverhead
```

所有计算必须使用 `resource.Quantity`，直到最终判断可分配的整数扩展资源单位，避免 milli、DecimalSI 或 BinarySI 转换丢失。

### 9.3 防止跨 Pool 重复容量

Pool 是筛选和策略视图，不是物理容量副本。若 `pool-a` 与 `pool-b` 都引用 `node-a/example.com/gpu-share`，两者必须共享同一占用视图。

记账主键建议为：

```text
LocalCapacityKey = NodeName + ResourceName
```

资源形态、profile 和 Pool 只能作为候选过滤条件，不能把同一 Node/resourceName 复制为多份容量。

### 9.4 远端库存

远端库存应综合：

- Endpoint 静态配置；
- Configured/Ready Condition 及 observedGeneration；
- 设备健康状态；
- 非终态 AcceleratorLease；
- 独占 Endpoint claim；
- Adapter 的地址、版本、API 和参数校验。

Inventory 缓存只用于候选发现，不是最终所有权依据。

## 10. PolicyEngine 设计

### 10.1 过滤顺序

建议按以下顺序过滤：

1. Provider/Pool 配置健康；
2. placement mode；
3. device type；
4. vendor/model；
5. resource form/profile/isolation；
6. memory 和属性；
7. PoolSelector 和 Node/Endpoint 标签；
8. readiness；
9. 可用容量；
10. S2 失败目标排除。

### 10.2 排序规则

同一模式内建议使用稳定排序：

1. Pool priority 降序；
2. 最佳适配容量，减少碎片；
3. Pool 名称；
4. Node/Endpoint 稳定 ID。

`Auto` 默认先 Local 后 Remote。S2 重试时可将相反模式作为软优先级，但失败过的具体 Node/Endpoint 必须作为硬排除。

### 10.3 模式行为

- `LocalOnly`：即使远端有容量也不得选择 Remote；
- `RemoteOnly`：本地结果不得成为最终选择；
- `Auto`：按 order 依次尝试；
- 未指定 order：默认 `[Local, Remote]`；
- order 不允许重复值；
- 启用 S2 时必须允许至少两种模式。

## 11. 本地 Adapter 设计

### 11.1 通用 Device Plugin Adapter

通用本地 Adapter 应适用于标准 Kubernetes Extended Resource：

- 从 Lease 快照读取 resourceName 和 count；
- 将相同 Quantity 写入目标容器 requests 和 limits；
- 将 Pod 约束到已选择 Node；
- 不注入 `LD_PRELOAD`、远端地址或远端凭据；
- 不选择物理 UUID；
- 将最终设备选择交给 kubelet 和 Device Plugin。

### 11.2 整卡

整卡示例：

```yaml
local:
  resourceName: nvidia.com/gpu
  resourceForm: WholeDevice
```

设计只依赖资源名，不应在 Controller 中假设 NVIDIA。AMD、Intel、昇腾或其他厂商只要遵守相同扩展资源语义，也可以使用独立 driver 注册同类 Adapter。

### 11.3 MIG

MIG 应建模为独立扩展资源和固定 profile，例如：

```yaml
local:
  resourceName: nvidia.com/mig-1g.10gb
  resourceForm: MIG
  profile: 1g.10gb
  memory: 10Gi
  isolation: hardware-partition
```

控制面只消费已由外部组件创建并发布的 MIG 实例。MIG Manager、重配置策略、碎片整理和物理拓扑属于独立运维/数据面设计。

### 11.4 软件共享资源

若共享方案把一个份额发布为单一扩展资源，可使用：

```yaml
local:
  resourceName: example.com/gpu-share
  resourceForm: Shared
  profile: 5Gi-share
  isolation: software
```

这里的 `count: 1` 表示一个插件定义的份额，不表示一张物理 GPU。

### 11.5 HAMi 专用设计注意事项

真实 HAMi 接入前必须固定 HAMi 版本并完成协议审计，确认：

- 实际发布哪些 resourceName；
- 设备数、显存、算力是否由多个关联 Quantity 表达；
- 是否依赖 mutating webhook、schedulerName、annotation 或 scheduler extender；
- 节点健康、设备拓扑和分配结果从何处读取；
- Pod 删除、节点重启和设备故障时如何回收；
- profile 是否能稳定映射为一个可审计的资源单位。

如果 HAMi 使用多维资源，不能把它强行压缩为单一 count。应引入 Adapter 专用的 `ResourceClaimProfile` 或类型化资源列表：

```yaml
localProfile:
  resources:
    vendor.example/device: "1"
    vendor.example/memory: "5Gi"
    vendor.example/cores: "50"
  schedulerName: hami-scheduler
  annotations: {}
```

相应 Lease 必须快照完整 ResourceList，RuntimePlan 必须原样物化，Inventory 必须避免把彼此关联的多维容量独立超卖。若 HAMi 自己是最终设备感知调度器，控制面应允许其成为最终权威，而不是重复实现同一调度算法。

## 12. 远端 Adapter 接口设计

本节描述的是稳定接口基线。当前仓库已在该接口之上提供 Cricket 真实数据面实现；新增其他
远端运行时仍应遵守同一 Provider/Allocator/RuntimePlan 边界。

远端 Adapter 应能够表达：

- Endpoint 或 session ID；
- runtime driver/version/API version；
- address 和协议参数；
- 可见设备及其属性；
- 独占 Endpoint 或设备级分配；
- TLS、认证和 Secret 引用；
- 客户端 runtime 镜像、init container、volume 和 mount；
- 运行时专用环境变量和 annotation；
- Reserve/Bind/Release 的幂等 session 语义。

通用层禁止出现具体 shim 文件名、固定路径、端口、RPC ID 或厂商环境变量。它们只能由具体 Provider 写入 RuntimePlan。

设备级远端分配必须同时满足：

- runtime 能按 session 隐藏未分配设备；
- Allocator 能对设备或 slice 做原子所有权控制；
- Endpoint binding 能记录每个设备的不可变标识；
- 数据面具备与声明一致的租户隔离；
- 测试能证明两个客户端不会看到或使用对方设备。

## 13. RuntimePlan 与 PodInjector

### 13.1 RuntimePlan

RuntimePlan 应使用类型化字段：

```go
type RuntimePlan struct {
    Mode             ExecutionMode
    ContainerName    string
    InitContainers   []corev1.Container
    Sidecars         []corev1.Container
    Volumes          []corev1.Volume
    Env              []corev1.EnvVar
    VolumeMounts     []corev1.VolumeMount
    Resources        *corev1.ResourceRequirements
    RequiredNodeName string
    SchedulerName    string
    Annotations      map[string]string
}
```

如当前阶段没有 sidecar 或 schedulerName 使用方，可暂不实现字段，但在接入确有需要的 Adapter 前必须先补充类型化能力和测试，不能退化为任意 JSON Patch。

### 13.2 模式不变量

Local RuntimePlan：

- 必须与 Lease 的 Local 模式一致；
- 必须请求 Lease 快照中的本地资源；
- 必须约束到选中 Node；
- 不得包含受管远端环境变量、shim、Endpoint 或远端 Secret。

Remote RuntimePlan：

- 必须与 Lease 的 Remote 模式一致；
- 必须来自不可变 Endpoint/session binding；
- 不得请求任何受管本地设备扩展资源；
- 不得同时设置本地设备 Node 绑定。

### 13.3 冲突策略

PodInjector 对以下情况必须报 Conflict：

- 同名环境变量值不同；
- 同名 volume 定义不同；
- 同一 mountPath 指向不同 volume；
- 同名 init container/sidecar 定义不同；
- 用户已声明同一受管 resourceName，但 Quantity 不同；
- NodeName、NodeSelector 或硬亲和性与选择目标冲突；
- 受管 annotation 已存在且值不同。

完全相同的再次应用必须幂等，不应重复追加字段。

## 14. 资源预留与并发一致性

### 14.1 先预留后创建 Pod

必须遵守：

```text
Create AcceleratorLease
  -> Allocator.Reserve
  -> Build/Validate RuntimePlan
  -> Create Pod
  -> Allocator.Bind
```

不能先创建 Pod 再补租约，否则 Pod 可能在所有权尚未建立时开始使用资源。

### 14.2 本地所有权

本地实际设备所有权最终由 kube-scheduler、kubelet 和 Device Plugin 决定。控制面 Lease 用于提前记账和避免自身重复选择，但不能伪造 kubelet 分配结果。

需要重点处理：

- 两个 Controller 副本基于相同旧库存同时选择；
- Pod 尚未调度时的乐观本地预留；
- 调度器最终拒绝后的释放或 S2；
- Lease 与已绑定 Pod 的防重复扣减；
- Pod 终态后的容量恢复。

若需要比 kube-scheduler 更强的本地原子预留，应使用调度器 Reservation、DRA ResourceClaim 或 Adapter 专用原子 claim，而不是仅依赖 Controller 缓存。

### 14.3 远端所有权

静态独占 Endpoint 建议使用 Coordination Lease 作为 CAS：

1. claim 名称由 Endpoint 稳定哈希；
2. holder 使用 AcceleratorLease UID；
3. Create 成功代表获得所有权；
4. 相同 holder 重试返回成功；
5. 不同存活 holder 返回 Conflict；
6. 原 owner 不存在、终态或 UID 不一致时才允许回收；
7. Release 校验 holder、UID 和 resourceVersion。

### 14.4 不可变快照

选择后不能依赖可变 Pool/Endpoint 重新解释已有 Lease。需要快照：

- 本地 resourceName、Quantity、Node、form、profile、isolation、attributes；
- 远端 Endpoint、address、runtime、devices、parameters 和 session；
- Workload generation 和 owner UID。

## 15. Workload 状态机

建议状态流：

```text
Pending
  -> Selecting
  -> Reserving
  -> Reserved
  -> RuntimePreparing
  -> Starting
  -> Running
  -> Releasing
  -> Released
```

API 可以合并部分外部 phase，但 Controller 内部必须保持这些语义边界。

### 15.1 Pending/Selecting

- 校验 Workload generation；
- 读取候选并运行 Policy；
- 无容量时设置 Unschedulable Condition 并监听相关资源变化；
- 不应在内存中保存唯一决策事实。

### 15.2 Reserving/Reserved

- 先创建确定性命名的 Lease；
- 若 Create 返回 AlreadyExists，必须校验 owner UID 和 generation；
- Reserve 对相同 LeaseKey 幂等；
- 预留失败不得创建业务 Pod。

### 15.3 RuntimePreparing/Starting

- 从已绑定 Lease 构建 RuntimePlan；
- 校验模式边界和 Pod 冲突；
- 使用确定性 Pod 名称创建；
- Create 超时属于结果未知，应查询同名 Pod，不能立即释放；
- 创建成功后 Bind，观察 Ready 和启动失败。

### 15.4 Running

- 第一次 Ready 必须持久化为单调状态；
- 后续 Ready=False 不得重新打开 S2；
- Endpoint/设备退化可以报告 Degraded，但基础设计不执行热迁移；
- Pod 终态进入释放。

### 15.5 Releasing/Released

- 若 Pod 仍可能运行，不能释放资源；
- Workload 删除、Lease 删除和 Pod 终态都必须汇合到同一幂等清理路径；
- Release 成功后再移除 finalizer；
- 直接删除 Lease 时应先处理关联 Pod。

## 16. S2 启动回退设计

### 16.1 触发边界

S2 只允许发生在该 Workload 生成的 Pod 第一次 `Ready=True` 之前。

可立即触发的故障建议包括：

- Reserve 返回容量冲突或 Unschedulable；
- RuntimePlan 准备阶段出现可重试资源冲突；
- scheduler 明确报告 Unschedulable；
- Adapter 注入的 runtime init container 拉取、创建、启动或退出失败；
- 启动期间选中 Endpoint 失效；
- 用户配置的 startupTimeout 到期。

不应立即触发：

- 应用容器镜像错误；
- 用户 init container 错误；
- 应用进程非零退出；
- 用户主动删除 Pod；
- 永久配置错误；
- Pod 曾经 Ready 后的任何退化。

### 16.2 持久化状态

Workload status 应保存：

- failedAttempts；
- maxAttempts；
- readyObserved；
- startupStartedAt；
- requeueAt；
- 每次失败的 mode、Provider、Pool、targets、PodRef、时间、reason 和 message。

Controller 不得用进程内计数器作为重试预算。

### 16.3 清理顺序

```text
记录失败历史
  -> 标记 CleanupPending
  -> 删除旧 Pod
  -> 等待 Pod NotFound
  -> Allocator.Release
  -> 移除旧 Lease finalizer
  -> 持久化 backoff deadline
  -> 清除 active allocation
  -> 排除失败目标并优先相反模式
```

任一步骤重试都必须幂等。资源不能在旧 Pod 仍可能使用时重新分配。

## 17. Controller 设计

建议拆分以下 Reconciler：

| Reconciler | 职责 |
| --- | --- |
| WorkloadReconciler | 选择、预留、Pod 物化、绑定、Running、S2 和回收 |
| ProviderReconciler | driver 与能力配置校验，发布 Ready |
| PoolReconciler | 引用、模式和资源配置校验，汇总容量 |
| EndpointReconciler | 静态结构和 runtime 兼容性校验，发布 Configured |
| HealthObserver | 可选；主动探测 Endpoint 并维护 Ready/lastProbeTime |

Watch 关系应覆盖：

- Workload 自身；
- 其 Lease 和 Pod；
- Provider/Pool 配置变化；
- Local Node/Pod 资源变化；
- Remote Endpoint 和 ownership Lease；
- 配置修复后重新触发此前 Unschedulable/Invalid 的 Workload。

## 18. Kubernetes 部署设计

### 18.1 控制面部署

建议提供：

- 非 root manager 镜像；
- Deployment，生产环境至少两个副本；
- leader election；
- readiness/liveness probe；
- ServiceAccount 和最小 RBAC；
- CRD、namespace、PDB 和 Kustomize；
- 可配置日志级别、controller namespace 和 metrics 地址；
- NetworkPolicy 和 Pod Security 配置建议。

### 18.2 开发集群

开发验证可分两条路径：

1. **WSL CPU 集群**：部署单节点 Kubernetes、CRD、manager 和 fake Device Plugin，验证 API、调度、Lease、Pod 生命周期和回收；
2. **原生 Linux GPU 集群**：验证 NVIDIA Driver、Container Toolkit、Device Plugin、MIG/HAMi 和真实 CUDA。

CPU 集群不应被用于宣称 GPU 隔离或 HAMi 数据面有效。

### 18.3 HAMi 部署前置条件

在部署 HAMi 做功能验收前，需要：

- 目标节点存在 HAMi 支持的真实设备；
- 宿主驱动和容器 runtime 可用；
- 容器内基础 GPU 测试通过；
- Kubernetes Node 能稳定运行 Device Plugin；
- 已确定 HAMi 版本、安装方式和资源协议；
- 已准备可重复的共享/显存/算力测试工作负载。

只在无 GPU 集群安装 HAMi 清单，最多能验证 Kubernetes 对象和基础控制面启动，不能验证切分功能。

## 19. 安全设计

### 19.1 Kubernetes 权限

Controller 应限制为：

- 读写本项目 CRD；
- 读 Node，读写受管 Pod；
- 创建 Event；
- 在专用命名空间读写 Coordination Lease；
- 读取显式允许的 Secret 引用；
- 不允许任意读取所有 namespace Secret。

### 19.2 Secret 处理

- RuntimePlan 只携带 SecretReference 或 volume/env 引用；
- status、log 和 Event 不得出现 Secret 值；
- Adapter 参数必须区分普通参数和敏感参数；
- 删除 Workload 后相关临时凭据/session 应按所属 Adapter 回收。

### 19.3 远端安全边界

远端运行时若不支持 TLS、认证和租户隔离，只能在明确的可信网络中用于实验。生产准入必须包含身份、加密、授权、审计、凭据轮换和服务端隔离测试。

## 20. 可观测性设计

### 20.1 Conditions

建议 Conditions：

- Workload：Selected、Reserved、RuntimeReady、Ready、Degraded、Released；
- Lease：Reserved、Bound、RuntimeReady、Released；
- Provider/Pool：Ready；
- Endpoint：Configured、Ready。

每个 Condition 应包含 observedGeneration、reason、message 和 transitionTime。

### 20.2 Events

至少记录：

- Selected/FallbackSelected；
- Reserved/ReservationConflict；
- RuntimePrepared/RuntimeConflict；
- PodCreated/Bound/Ready；
- StartupFailed/FallbackScheduled/FallbackExhausted；
- Degraded；
- Released；
- ConfigurationReady/ConfigurationInvalid。

### 20.3 Metrics

计划指标：

- selection 次数和延迟；
- 按模式/原因统计的选择结果；
- reserve/bind/release 延迟和失败；
- S2 次数、成功率和耗时；
- active Lease、orphan claim 和 cleanup backlog；
- Pool total/available/reserved；
- reconcile error、workqueue depth 和 retry；
- Pod startup latency。

## 21. 测试设计

### 21.1 单元测试

至少覆盖：

1. 整卡选择与注入；
2. MIG resourceName/profile；
3. 非 NVIDIA 共享资源名；
4. Auto 本地不足选择 Remote；
5. LocalOnly 不选择远端；
6. RemoteOnly 忽略本地；
7. Local -> Remote S2；
8. Remote -> Local S2；
9. Ready 后禁止回退；
10. 应用镜像错误不立即回退；
11. 受管 runtime init 失败触发 S2；
12. 跨 Pool 相同 resourceName 防超卖；
13. Lease/Pod 防重复扣减；
14. 两个 Workload 争用独占 Endpoint；
15. 未注册 Adapter；
16. 重复 driver；
17. 缺失 Provider/Allocator；
18. Local plan 无远端材料；
19. Remote plan 无本地资源；
20. 已存在 Pod 的 reconcile 幂等；
21. Workload/Lease/Pod 三类释放；
22. 重启后失败历史和 deadline；
23. env/volume/resource/node 冲突；
24. 新本地 Adapter 不修改 Controller。

### 21.2 envtest

envtest 应验证：

- CRD 安装和 OpenAPI schema；
- 默认值、枚举和 CEL 规则；
- Lease spec 不可变；
- ownerReference/finalizer 行为；
- Coordination Lease CAS；
- status subresource；
- RBAC 所需资源范围。

### 21.3 CPU Kubernetes E2E

使用 fake Device Plugin 发布测试扩展资源，验证：

- manager 部署和 leader election；
- CRD/RBAC/Kustomize；
- 真实 scheduler/kubelet 的 extended resource 调度；
- 两个 Pool 共享资源名；
- Pod 创建、绑定、终态与删除；
- Controller 重启和多副本切换；
- API timeout、Pod 删除阻塞和 finalizer 恢复。

### 21.4 真实 GPU/HAMi E2E

分层执行：

1. 宿主 `nvidia-smi`；
2. 容器内 GPU；
3. 原生 Device Plugin 整卡 Pod；
4. 控制面 WholeDevice；
5. 静态 MIG profile；
6. HAMi 单份额；
7. 多 Pod 共享、显存和算力限制；
8. 超额申请和隔离；
9. Pod 删除、节点重启和设备故障；
10. Controller/HAMi scheduler 重启恢复。

真实测试必须同时观察 Kubernetes 资源、容器可见设备、显存限制、计算限制和隔离，不得只以 Pod Running 作为成功标准。

## 22. 实施阶段与任务拆分

本文不记录阶段完成状态。所有阶段均应通过独立交付记录确认。

### 阶段 A：API 与工程基线

需要完成：

- 定义五类 CRD、status、Conditions 和校验规则；
- 建立 controller-runtime manager；
- 生成 DeepCopy 和 CRD；
- 编写 RBAC、Dockerfile 和 Kustomize；
- 建立错误分类、日志和 Event 规范；
- 建立单元测试与 envtest 工具链。

验收：CRD 可安装、manager 可启动、schema/defaulting/immutability 测试通过。

### 阶段 B：通用本地 S1

需要完成：

- InventoryReader；
- `resource.Quantity` 记账；
- LocalFirst Policy；
- ExecutionAdapter Registry；
- 通用本地 Provider/Allocator；
- RuntimePlan/PodInjector；
- Workload/Lease 基础状态机；
- 整卡、MIG 和单资源共享份额测试。

验收：无 GPU 单元测试和 fake Device Plugin 集群 E2E 通过。

### 阶段 C：远端接口和一致性边界

需要完成：

- 通用 Endpoint、Resolver 和 Validator 接口；
- 远端不可变 binding；
- Endpoint/session 原子 claim；
- Remote RuntimePlan 模式边界；
- mock 远端 Adapter 测试。

原始路线图阶段不包含真实 CUDA 远端 runtime；该项已由 Cricket Adapter 和独立的真实 CUDA
E2E 完成。后续阶段应关注健康探针、凭据、生命周期和多租户安全，而不是重复实现通用选择层。

### 阶段 D：S2

需要完成：

- 失败分类；
- durable history、attempt budget、backoff；
- 失败目标排除和反向模式偏好；
- 清理顺序；
- Controller 重启恢复；
- 双向回退测试。

验收：24 项核心控制面场景和故障恢复矩阵通过。

### 阶段 E：真实本地 GPU

需要完成：

- 原生 Linux GPU 集群；
- Driver/Container Toolkit/Device Plugin；
- 整卡 CUDA E2E；
- MIG 配置与 profile；
- 性能和故障基线。

### 阶段 F：HAMi Adapter

需要完成：

- 固定 HAMi 版本并审计资源协议；
- 决定复用通用 Adapter 或新增专用 Adapter；
- 多维 ResourceList/调度器/annotation 建模；
- HAMi Inventory 与 RuntimePlan；
- 显存/算力/共享隔离和回收测试；
- 文档化支持矩阵和限制。

### 阶段 G：生产化

需要完成：

- HA 和 leader 故障注入；
- metrics、dashboard、alert 和 SLO；
- quota、admission 和安全策略；
- 升级、回滚、备份和事故手册；
- 镜像供应链、签名和漏洞扫描；
- 容量与性能压测。

## 23. 验收标准

系统进入控制面验收时，应满足：

- Controller 不依赖某个具体本地或远端 runtime；
- 本地资源名只来自配置或 Lease 快照；
- WholeDevice、MIG、Shared、Custom 进入同一选择流程但保持不同语义；
- Local/Remote RuntimePlan 严格互斥；
- Provider/Allocator 不会跨 driver 错配；
- Reserve/Bind/Release 对 LeaseKey 幂等；
- 缓存过期不会破坏最终独占所有权；
- Pod Create 结果未知时不会提前释放；
- S2 只在第一次 Ready 前发生；
- 应用错误不会默认解释为设备错误；
- Controller 重启不丢失决策、预算、历史或 deadline；
- 所有资源清理路径最终收敛且不会在 Pod 仍可能运行时释放；
- 不使用真实 GPU也能完成核心控制面测试。

系统进入本地数据面验收时，还应满足：

- 真实整卡、MIG 或 HAMi 资源能被 Node 正确发布；
- Pod 请求与实际可见设备一致；
- 共享显存和算力限制可测量；
- 不同租户不能越权访问设备或份额；
- 节点、设备、kubelet、Device Plugin 和调度器故障能够安全恢复；
- 性能、稳定性和兼容矩阵达到目标环境要求。

## 24. 风险与待决策项

| 风险/决策 | 需要确认的内容 |
| --- | --- |
| HAMi 资源协议 | 目标版本、资源键、多维 Quantity、scheduler/webhook 依赖 |
| MIG 生命周期 | 静态配置还是动态重配置，谁负责碎片整理 |
| 本地原子预留 | kube-scheduler 是否足够，是否引入 Reservation/DRA |
| 多资源关联 | 设备数、显存、算力如何形成不可分割 claim |
| 远端分配粒度 | Endpoint 独占还是 session/device/slice |
| 远端安全 | TLS、认证、网络隔离和租户边界 |
| NPU 范围 | 首个目标厂商、Device Plugin/DRA、SDK 和镜像 |
| 调度扩展 | 是否与 HAMi、Volcano、KAI 或原生 scheduler 组合 |
| CRD 演进 | v1alpha1 到稳定版本的兼容、转换和回滚 |
| 容量权威 | Controller、scheduler、Device Plugin 各自负责哪一层事实 |

所有待决策项都应在实现相应阶段前形成 ADR（Architecture Decision Record），避免把厂商特例隐式写入通用控制器。

## 25. 交付物清单

完整交付应包含：

- API/CRD 设计与生成清单；
- ExecutionAdapter 接口及至少一个本地实现；
- Inventory、Policy、Allocation、RuntimePlan 和 Injector；
- Workload/Lease Controller 与配置 Controller；
- RBAC、镜像和 Kustomize；
- WholeDevice、MIG、Shared 示例；
- 24 项核心场景测试；
- envtest 和 CPU 集群 E2E；
- 真实 GPU/HAMi 测试报告；
- Adapter 开发指南；
- 部署、升级、回滚和故障处理手册；
- 风险清单、ADR 和支持矩阵。

## 26. 设计结论

本方案应以 Kubernetes 持久化对象作为恢复事实，以 `ExecutionAdapter` 作为执行底座边界，以不可变 Lease 作为资源选择和绑定记录，以 RuntimePlan 作为控制面与数据面的物化边界。

本地整卡、MIG 和共享份额可以共享选择、租约和状态机框架，但不能共享未经证明的容量单位与隔离假设。HAMi 应在确认具体版本和资源协议后，以配置化通用 Adapter 或专用 Adapter 接入。远端 runtime 只通过受约束接口扩展，不进入通用 Controller。

最终应先证明控制面一致性和恢复能力，再分别对真实 Device Plugin、MIG、HAMi、CUDA 和厂商数据面执行独立验收。设计文档本身不代表任何阶段已经实施或通过验收。
