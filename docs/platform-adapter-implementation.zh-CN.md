# 异构加速资源平台适配层：总体架构、设计原理与模拟验证

## 1. 文档定位

本文面向第一次接触 accelerator-control-plane 项目的读者，目标不是逐个解释 Go 包，而是先回答四个更重要的问题：

1. 这个平台要解决什么问题；
2. 本地资源和远端资源为什么能够用同一套方式表达；
3. 平台怎样在“仅本地、仅远端、自动选择”之间作出可解释的决定；
4. 在尚未接入真实容器执行和真实远端计算后端时，怎样通过模拟实验验证控制面逻辑是正确的。

本文的核心结论是：

> 平台适配层已经建立了统一任务入口、统一资源模型、候选资源盘点、模式选择、容量预留、决策持久化、运行计划生成以及资源释放的控制面主链。

这里的“控制面”可以理解为平台的大脑。它负责回答“任务需要什么、有哪些资源、应该选哪一个、容量属于谁、后续应怎样启动”。

真正执行程序、拉取镜像、调用 GPU 或连接远端运行时的部分称为“数据面”。数据面不属于本文当前验收范围。

### 1.1 当前正式验收范围

本文验证以下能力：

- 用户能够用统一对象声明加速资源需求；
- 平台能够把本地节点和模拟远端端点整理为统一候选资源；
- 平台支持仅本地、仅远端和自动选择三种模式；
- 自动模式默认采用“本地优先，本地不足时再考虑远端”的顺序；
- 选择结果会被保存为调度决策；
- 容量会被保存为资源租约，避免多个任务重复占用；
- 控制器重复执行或重建后能够依据持久化对象继续工作；
- 任务结束或删除后，控制面容量能够释放；
- 本地执行计划能够正确表达节点约束和 Kubernetes 扩展资源请求；
- 模拟远端端点能够参与选择和排他占用测试。

### 1.2 当前不作完成声明的内容

本文不声称以下能力已经完成：

- 真实业务容器已经由平台稳定运行；
- 真实 GPU 程序已经通过平台完成计算；
- 任意远端 GPU 调用技术已经正式接入平台；
- 平台已经实现运行中的本地与远端热迁移；
- 平台已经完成生产级认证、加密、配额和多租户隔离；
- 模拟远端端点等同于真实远端服务器。

因此，本文中的“远端”表示控制面中的远端资源抽象或模拟端点，不表示已经连接某个真实远程计算系统。

### 1.3 推荐阅读顺序

第一次阅读时建议按以下顺序：

1. 第 2～4 章：理解问题、总体架构和本地/远端选择；
2. 第 5～7 章：理解一次任务的完整生命周期和核心对象；
3. 第 8 章：按照代码路径定位实现；
4. 第 9～14 章：执行场景化模拟实验并采集截图；
5. 第 15～16 章：理解当前边界和后续接入方向。

### 1.4 常用术语的中文含义

本文会保留代码中真实使用的英文名称，但第一次出现时按下表理解即可：

| 英文术语 | 中文解释 | 在本项目中的含义 |
|---|---|---|
| Control Plane | 控制面 | 接收任务、盘点资源、作出选择、保存租约并推进状态 |
| Data Plane | 数据面 | 真正启动程序、传输数据并使用 GPU 计算 |
| Workload | 工作负载/任务 | 用户提交的一次加速计算需求 |
| Inventory | 资源盘点 | 收集并计算当前可使用的本地和远端资源 |
| Candidate | 候选资源 | 经过统一格式整理、可供策略比较的一项资源 |
| Policy | 选择策略 | 决定使用哪种模式、资源池和目标的规则 |
| Decision | 调度决策 | 平台保存的选择结果和选择原因 |
| Lease | 资源租约 | 某个任务对一份容量的持久化所有权凭证 |
| RuntimePlan | 运行计划 | 把已确定目标翻译成后续启动所需的资源和环境配置 |
| Provider | 能力提供者 | 校验一种后端，并根据 Lease 生成运行计划 |
| Allocator | 资源分配器 | 负责预留、绑定和释放该后端的容量 |
| Adapter | 适配器 | 同一个后端的 Provider 与 Allocator 的组合 |
| Endpoint | 资源端点 | 控制面中代表一个远端运行入口的对象 |
| Reconcile | 调谐 | Controller 重复检查实际状态并向期望状态推进的过程 |
| Finalizer | 终结器 | 防止对象在资源释放完成前直接被删除的保护标记 |
| Unschedulable | 当前不可调度 | 现有候选无法完整满足任务需求 |

## 2. 平台要解决的实际问题

### 2.1 没有统一平台时会发生什么

如果每个业务直接面向底层资源编写逻辑，通常会形成两套甚至多套流程：

~~~text
本地 GPU 任务
  -> 查 Kubernetes 节点
  -> 识别 Device Plugin 资源名
  -> 修改 Pod resources
  -> 等待调度

远端 GPU 任务
  -> 查远端服务器或服务端点
  -> 判断设备是否可用
  -> 处理地址和连接参数
  -> 启动对应客户端
~~~

这样会带来几个问题：

- 业务代码必须知道底层资源类型；
- 更换后端时需要改业务提交方式；
- 本地和远端容量无法统一比较；
- 多个任务竞争同一远端端点时容易重复分配；
- 控制器重启后难以恢复之前的选择；
- 无法清楚说明“为什么选择本地”或“为什么转向远端”；
- 资源释放和异常清理容易散落在不同实现中。

### 2.2 平台适配层的核心思路

平台不要求用户直接选择某个具体节点或某台远端服务器，而是让用户提交一份资源意图：

~~~yaml
spec:
  accelerator:
    deviceType: GPU
    vendor: nvidia
    count: 1
  placement:
    mode: Auto
    order: [Local, Remote]
~~~

这段配置的中文含义是：

~~~text
我要 1 个 NVIDIA GPU；
允许平台自动选择；
优先使用本地资源；
本地不能满足时，再考虑远端资源。
~~~

平台随后完成：

~~~text
任务已受理
  -> 资源盘点
  -> 模式过滤
  -> 策略选择
  -> 容量预留
  -> 执行计划就绪
  -> 资源释放
~~~

### 2.3 平台的价值不只是“自动选择”

自动选择只是最直观的一层。更重要的是，平台把一次选择变成了可保存、可追踪、可恢复的控制面记录：

| 问题 | 平台对象或机制 |
|---|---|
| 用户申请了什么 | AcceleratorWorkload |
| 当时有哪些资源 | Inventory 资源盘点 |
| 为什么选择某个资源 | Decision 调度决策 |
| 资源现在属于谁 | AcceleratorLease 资源租约 |
| 后续应该怎样启动 | RuntimePlan 运行计划 |
| 控制器重启后怎样继续 | 持久化状态与幂等调谐 |
| 任务删除后怎样归还容量 | Finalizer 与 Release |

## 3. 总体架构

### 3.1 一张图理解整个平台

~~~text
┌──────────────────────────────────────────────────────────┐
│                    用户提交统一任务                       │
│ AcceleratorWorkload：设备类型、数量、属性、放置模式       │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│                    任务控制器                             │
│ 校验任务、推进状态、协调盘点/选择/预留/释放                │
└───────────────┬──────────────────────────┬───────────────┘
                │                          │
                ▼                          ▼
┌──────────────────────────┐  ┌───────────────────────────┐
│       统一资源盘点        │  │         选择策略          │
│ 本地 Node -> Candidate    │  │ LocalOnly / RemoteOnly   │
│ 远端 Endpoint -> Candidate│  │ Auto：默认本地优先       │
└───────────────┬──────────┘  └─────────────┬─────────────┘
                └──────────────┬─────────────┘
                               ▼
┌──────────────────────────────────────────────────────────┐
│                    Decision 调度决策                      │
│ 模式、Provider、Pool、目标、策略名、选择原因               │
└──────────────────────────┬───────────────────────────────┘
                           ▼
┌──────────────────────────────────────────────────────────┐
│                 AcceleratorLease 资源租约                 │
│ 保存不可变目标、预留容量、阻止重复占用、支持恢复与释放      │
└──────────────────────────┬───────────────────────────────┘
                           ▼
┌──────────────────────────────────────────────────────────┐
│                    RuntimePlan 运行计划                   │
│ 本地：资源请求和节点约束                                  │
│ 远端：未来由具体远端适配器提供连接与运行参数               │
└──────────────────────────┬───────────────────────────────┘
                           ▼
┌──────────────────────────────────────────────────────────┐
│                  后续真实执行层（未验收）                 │
│ Kubernetes Pod / 主机进程 / WSL 进程 / 外部作业系统       │
└──────────────────────────────────────────────────────────┘
~~~

这张图中最重要的分界是：

- RuntimePlan 之前属于本文验证的控制面；
- 真正启动程序和执行计算属于后续数据面。

### 3.2 平台不是 kube-scheduler 的替代品

平台负责选择“使用哪类加速资源、哪个资源池、哪个逻辑目标”。

如果最终选择本地 Kubernetes 资源，实际 Pod 的节点绑定仍需遵守 Kubernetes 的调度语义。平台生成资源请求和必要约束，kube-scheduler 仍负责通用 Pod 调度。

因此二者关系是：

~~~text
平台控制面：选择加速资源并形成计划
Kubernetes：执行本地 Pod 的通用调度与生命周期
~~~

### 3.3 适配器在架构中的位置

不同后端通过适配器接入统一控制面。一个完整适配器包含两部分：

| 组成 | 中文职责 | 核心方法 |
|---|---|---|
| Provider | 能力提供与运行计划生成 | Validate、BuildRuntimePlan |
| Allocator | 容量占用与释放 | Reserve、Bind、Release |

Provider 和 Allocator 必须属于同一个 driver 边界。平台控制器只依赖统一合同，不应该直接调用 CUDA、RPC 或厂商 SDK。

对应代码：

- 统一合同：internal/contracts/contracts.go
- 适配器注册表：internal/adapter/registry.go
- 控制器入口：internal/controller/workload_controller.go

## 4. 本地与远端如何选择

### 4.1 先把不同资源翻译成同一种候选

平台内部使用 Candidate（候选资源）描述可选择的容量。

本地 Candidate 通常代表一个 Kubernetes Node 上某类可分配资源：

~~~text
mode          = Local
pool          = local-gpu
node          = node-a
resourceName  = nvidia.com/gpu
available     = 1
vendor        = nvidia
model         = A100
ready         = true
~~~

模拟远端 Candidate 通常代表一个逻辑 Endpoint（端点）：

~~~text
mode          = Remote
pool          = mock-remote-gpu
endpoint      = endpoint-a
available     = 1
deviceIDs     = [gpu-0]
exclusive     = true
ready         = true
~~~

二者的来源不同，但进入策略层以后都能接受以下过滤：

- 设备类型是否一致；
- 厂商和型号是否匹配；
- 数量是否足够；
- 显存是否足够；
- WholeDevice、MIG、Shared 等资源形态是否匹配；
- profile 和 isolation 是否匹配；
- 标签和扩展属性是否匹配；
- 候选是否 Ready；
- 候选是否被标记为不可调度；
- 候选是否已经被租约占用。

对应代码：

- 候选定义：internal/contracts/contracts.go 中的 Candidate
- 资源盘点：internal/inventory/reader.go
- 容量核算：internal/inventory/accounting.go
- 容量摘要：internal/inventory/capacity.go

### 4.2 三种放置模式

| 用户模式 | 中文含义 | 选择范围 | 不满足时的结果 |
|---|---|---|---|
| LocalOnly | 仅本地 | 只看 Local Candidate | 等待或不可调度，不会偷偷改用远端 |
| RemoteOnly | 仅远端 | 只看 Remote Candidate | 等待或不可调度，不会偷偷改用本地 |
| Auto | 自动选择 | 按 order 比较允许的模式 | 当前模式不满足时检查下一个模式 |

模式定义位于：

- api/v1alpha1/common_types.go 中的 PlacementMode
- api/v1alpha1/acceleratorworkload_types.go 中的 PlacementPolicy

### 4.3 Auto 的当前规则

如果用户没有指定 order，当前默认顺序为：

~~~text
[Local, Remote]
~~~

也就是：

~~~text
本地资源满足全部条件
  -> 选择本地

本地资源不存在、容量不足或属性不匹配
  -> 检查模拟远端候选

模拟远端候选满足全部条件
  -> 选择远端抽象

两边都不能满足
  -> 返回 Unschedulable（当前不可调度）
~~~

策略代码位于 internal/policy/local_first.go，策略名称是 local-first-fit/v1。

### 4.4 “切换”在当前阶段的准确含义

当前已经实现的是选择阶段和启动前失败处理中的模式切换基础，而不是运行中热迁移。

当前可以表达：

~~~text
Auto 首次选择本地
  -> 如果在任务真正 Ready 之前发生可归类的资源或启动准备失败
  -> 记录失败目标
  -> 释放原租约
  -> 排除失败目标
  -> 重新选择另一个允许模式
~~~

当前不能宣称：

~~~text
正在本地 GPU 上运行的程序
  -> 无停机迁移到远端 GPU
  -> 自动恢复执行上下文
~~~

后者需要进程状态、GPU 状态、检查点、网络和具体运行时共同支持，不属于当前控制面模拟验证。

### 4.5 四个直观例子

| 场景 | 本地容量 | 模拟远端容量 | 模式 | 预期结果 |
|---|---:|---:|---|---|
| A | 1 | 1 | LocalOnly | 选择本地 |
| B | 1 | 1 | RemoteOnly | 选择模拟远端 |
| C | 1 | 1 | Auto | 默认选择本地 |
| D | 0 | 1 | Auto | 选择模拟远端 |

再增加一个失败场景：

| 场景 | 本地容量 | 模拟远端容量 | 请求量 | 预期结果 |
|---|---:|---:|---:|---|
| E | 1 | 1 | 2 | 两边都不能完整满足，返回不可调度 |

平台不会为了“看起来成功”而悄悄减少申请数量。

## 5. 一次任务的完整生命周期

### 5.1 第一步：任务已受理

用户创建 AcceleratorWorkload，控制器首先校验：

- 设备类型是否合法；
- 数量是否大于零；
- Pod 模板是否完整；
- placement 配置是否有效；
- 指定容器是否存在；
- 任务是否已经进入不可修改阶段。

主要代码：

- api/v1alpha1/acceleratorworkload_types.go
- internal/controller/validation.go
- internal/controller/workload_controller.go 中的 Reconcile

### 5.2 第二步：资源盘点

Inventory（资源盘点器）读取 Provider、Pool、Node、Endpoint、Pod 和已有 Lease，计算当时可用的 Candidate。

本地可用量不能只看 Node.allocatable，还要扣除：

- 已经运行或绑定的 Pod 请求；
- 已创建但还没有完全进入运行状态的 AcceleratorLease；
- 共享同一扩展资源名的其他 Pool 预留。

这样可以避免“Pod 已经占用一次，Lease 又重复扣减一次”，也能避免新 Lease 尚未写入 status 时出现并发窗口。

### 5.3 第三步：模式过滤

Policy（选择策略）先根据 LocalOnly、RemoteOnly 或 Auto 确定允许的模式，再过滤不兼容候选。

例如 RemoteOnly 即使看到一个更高优先级的本地 Candidate，也必须忽略它。

### 5.4 第四步：策略选择

策略对剩余候选稳定排序，得到 Decision（调度决策）。Decision 至少保存：

- Local 或 Remote；
- Provider 名称；
- Pool 名称；
- 节点名或端点名；
- 设备 ID 或本地资源数量；
- 策略名称；
- 可读的选择原因；
- 决策时间。

Decision 不是临时日志，而是写入 Workload status 的持久化证据。

### 5.5 第五步：容量预留

控制器根据 Decision 创建 AcceleratorLease。

Lease 的中文含义是“资源租约”或“容量所有权凭证”。它保存不可变目标，回答：

~~~text
谁申请了资源？
申请的是哪一种资源？
最终选中了哪里？
租约属于本地还是远端？
由哪个 driver 负责释放？
租约何时过期？
当前处于 Pending、Reserved、Bound 还是 Released？
~~~

Lease 目标不可变非常重要。如果 Pool 或 Endpoint 后来被修改，已经作出的分配不能被悄悄重定向到另一台设备。

### 5.6 第六步：运行计划就绪

Provider 根据已经确定的 Lease 生成 RuntimePlan。

RuntimePlan 不是第二次调度。它不能重新选择目标，只能把既有决策翻译为启动参数。

本地计划可以包含：

~~~text
Resources          -> 容器需要的扩展资源
RequiredNodeName   -> 已选择的节点
ContainerName      -> 接收资源的业务容器
Annotations        -> 控制面元数据
~~~

远端计划未来可以包含环境变量、挂载或初始化步骤，但本文不声明某个真实远端运行时已经完成接入。

### 5.7 第七步：资源释放

任务结束、删除、租约过期或启动前失败时，控制器进入 Release 路径。

Finalizer（终结器）保证对象不会在资源清理前直接消失。释放成功后：

- Lease 进入 Released；
- 排他端点 claim 被删除；
- 本地或模拟远端容量重新可见；
- Workload 可以进入终态或完成删除。

主要代码：

- internal/controller/workload_controller.go 中的 releaseLease、finalizeWorkload
- internal/allocation/local.go
- internal/allocation/remote.go
- internal/allocation/kubernetes_claims.go

## 6. 五类核心对象

### 6.1 AcceleratorProvider：后端能力说明

Provider 表示一种资源后端或适配器类型，主要描述：

- driver 名称；
- 支持 GPU 还是 NPU；
- 支持 Local 还是 Remote；
- 支持 WholeDevice、MIG、Shared 还是 Custom；
- 是否支持某类远端分配方式；
- 如何校验配置并生成运行计划。

类型定义：api/v1alpha1/acceleratorprovider_types.go

### 6.2 AcceleratorPool：同类资源池

Pool 把同一类、可使用同一策略比较的容量组织在一起。

本地 Pool 通常保存：

- Kubernetes 扩展资源名；
- 节点选择标签；
- 资源形态；
- profile、isolation、vendor 等属性。

远端 Pool 通常保存：

- 端点选择标签；
- 排他端点或设备级分配方式；
- 资源池优先级。

类型定义：api/v1alpha1/acceleratorpool_types.go

### 6.3 AcceleratorEndpoint：模拟或真实远端资源入口

Endpoint 描述一个远端资源入口，其字段包括：

- 所属 Pool；
- 运行地址；
- runtime driver 和版本；
- 可见设备列表；
- 每个设备的型号、显存、健康状态和资源形态；
- 是否允许调度；
- 最近探测状态。

在本文实验中，Endpoint 只作为模拟控制面对象使用。

类型定义：api/v1alpha1/acceleratorendpoint_types.go

### 6.4 AcceleratorWorkload：用户任务意图

Workload 是用户入口，包含：

- accelerator：需要什么设备；
- placement：允许平台怎样选择；
- podTemplate：未来执行载荷的模板；
- status.decision：平台最终为什么这样选；
- status.allocationRef：对应哪个 Lease；
- status.conditions：Accepted、Selected、Reserved 等阶段证据。

类型定义：api/v1alpha1/acceleratorworkload_types.go

### 6.5 AcceleratorLease：资源所有权快照

Lease 保存一次已经作出的分配，包含：

- Workload 所有者；
- Workload generation；
- 原始设备请求；
- driver、Provider 和 Pool；
- Local 或 Remote；
- 不可变目标；
- TTL；
- 绑定信息和状态。

类型定义：api/v1alpha1/acceleratorlease_types.go

## 7. 正确性设计

### 7.1 为什么必须先有 Decision，再有 Lease

Decision 解释“为什么选它”，Lease 解决“它现在属于谁”。

如果只有 Decision 没有 Lease，两个并发任务可能都看到同一份空闲容量；如果只有 Lease 没有 Decision，则无法解释选择来源和策略。

### 7.2 为什么 Lease 从创建时就要消耗容量

新 Lease 可能刚创建，status 还没有初始化。如果容量核算只统计 Reserved 或 Bound 状态，这个短暂窗口会让另一个任务再次选中相同资源。

当前核算逻辑把尚未释放的新 Lease 也视为占用，从而缩小并发重复分配窗口。

验证代码：internal/inventory/accounting_test.go 中的 TestNewLeaseConsumesCapacityBeforeStatusInitialization。

### 7.3 为什么远端排他端点还需要原子 Claim

资源盘点是一个快照。即使两个任务几乎同时读取到“endpoint-a 空闲”，最终 Reserve 仍必须只有一个成功。

平台使用 Kubernetes coordination Lease 作为 Endpoint Claim 的原子所有权记录：

~~~text
任务 A Claim endpoint-a -> 成功
任务 A 重复 Claim        -> 幂等成功
任务 B Claim endpoint-a -> 冲突
任务 B 尝试释放         -> 拒绝，因为不是所有者
任务 A Release          -> 成功
任务 B 再次 Claim       -> 可以成功
~~~

代码路径：

- internal/allocation/kubernetes_claims.go
- internal/allocation/kubernetes_claims_test.go

### 7.4 为什么控制器必须幂等

Kubernetes Controller 的 Reconcile 会被重复调用。网络抖动、对象更新、Controller 重启都可能让同一个任务再次进入调谐。

因此同一个 LeaseKey 的 Reserve、Bind 和 Release 必须可重复执行，不能因为重试产生第二个 Pod、第二个租约或第二份容量扣减。

### 7.5 为什么要保存 driver 快照

Lease 中保存 driver 后，释放路径不必重新根据可能已经被删除或修改的 Provider/Pool 猜测后端。

这保证配置对象发生变化时，控制器仍知道应该调用哪一个 Allocator 清理原分配。

### 7.6 为什么运行计划不能跨模式混用

Local Lease 只能生成 Local RuntimePlan；Remote Lease 只能生成 Remote RuntimePlan。

如果本地计划意外带入远端环境变量，或者远端计划意外注入本地扩展资源，控制器必须拒绝，而不能生成含义不明的任务。

## 8. 项目代码路径导读

下表按一次任务的控制链排列，而不是按 Go 包字母顺序排列。

| 控制阶段 | 主要代码路径 | 阅读重点 |
|---|---|---|
| 用户任务模型 | api/v1alpha1/acceleratorworkload_types.go | 资源请求、placement、状态和失败历史 |
| 通用类型 | api/v1alpha1/common_types.go | Local/Remote、LocalOnly/RemoteOnly/Auto、Decision、Target |
| Provider/Pool/Endpoint/Lease | api/v1alpha1/*_types.go | 资源配置和持久化所有权 |
| 控制循环入口 | internal/controller/workload_controller.go | Reconcile 如何推进完整生命周期 |
| 输入校验 | internal/controller/validation.go | 拒绝不完整或冲突任务 |
| 启动前重新选择 | internal/controller/fallback.go | 失败历史、排除目标、回退时限 |
| 统一资源盘点 | internal/inventory/reader.go | Node 和 Endpoint 如何转为 Candidate |
| 容量核算 | internal/inventory/accounting.go | Pod 与 Lease 如何共同扣减容量 |
| 策略选择 | internal/policy/local_first.go | 模式过滤、本地优先、属性匹配和稳定排序 |
| 统一合同 | internal/contracts/contracts.go | Candidate、Decision、Provider、Allocator、RuntimePlan |
| 适配器注册 | internal/adapter/registry.go | driver 唯一性和 Provider/Allocator 绑定 |
| 本地分配 | internal/allocation/local.go | 本地 Reserve/Bind/Release 的幂等语义 |
| 端点占用 | internal/allocation/kubernetes_claims.go | 排他端点的原子 Claim |
| Pod 计划变更 | internal/injector/pod.go | 把 RuntimePlan 安全写入 PodTemplate |
| 本地计划生成 | internal/provider/local/provider.go | 本地资源请求和节点约束 |
| Manager 组装 | cmd/manager/main.go | Controller、Registry、Inventory、Policy 的装配入口 |

### 8.1 建议源码阅读路线

如果只想理解主链，建议按以下顺序：

~~~text
api/v1alpha1/acceleratorworkload_types.go
  -> internal/contracts/contracts.go
  -> internal/inventory/reader.go
  -> internal/policy/local_first.go
  -> internal/controller/workload_controller.go
  -> api/v1alpha1/acceleratorlease_types.go
  -> internal/provider/local/provider.go
  -> internal/injector/pod.go
~~~

## 9. 模拟实验设计

### 9.1 为什么先做模拟实验

控制面正确性不应该依赖现场是否恰好有 GPU、镜像仓库或远端服务。

模拟实验可以稳定验证：

- 相同输入是否总能产生相同选择；
- 不同模式是否严格遵守用户意图；
- 容量不足时是否拒绝错误分配；
- 两个任务是否会重复占用同一排他端点；
- Controller 重复调谐是否产生重复对象；
- 删除后容量是否真正归还；
- CRD schema 和 API Server 并发语义是否成立。

### 9.2 模拟层次

| 层次 | 使用工具 | 能证明什么 | 不能证明什么 |
|---|---|---|---|
| 策略模拟 | Go 单元测试 | LocalOnly、RemoteOnly、Auto 的选择结果 | 真实资源可连接 |
| fake client 控制器模拟 | controller-runtime fake client | Workload、Decision、Lease 和 Pod API 对象生命周期 | 真实 kubelet 和容器运行 |
| 原子占用模拟 | coordination Lease + fake client | 排他端点竞争、幂等和所有权 | 真实远端设备隔离 |
| Envtest | 真实 kube-apiserver 和 etcd 测试进程 | CRD、默认值、resourceVersion、原子写入 | scheduler、kubelet、GPU |
| server-side dry-run | 已有 Kubernetes API Server | YAML 能通过当前 CRD schema | 对象实际执行 |

### 9.3 实验环境记录

每次正式实验应记录：

~~~text
日期：
Git commit：
工作树状态：
Go 版本：
Kubernetes 或 Envtest 版本：
执行命令：
预期结果：
实际结果：
退出码：
日志路径：
截图编号：
~~~

实验截图必须来自真实终端。不要把重新排版的文字图片当作现场截图。

## 10. 模拟实验一：本地、远端与自动选择

### 10.1 实验目的

验证平台的三种模式不是说明文字，而是由实际策略代码执行：

- LocalOnly 只允许本地；
- RemoteOnly 只允许模拟远端；
- Auto 在本地和模拟远端都可用时选择本地；
- Auto 在本地容量不足时选择模拟远端；
- 两边都不足时返回不可调度。

### 10.2 对应实现

- 策略实现：internal/policy/local_first.go
- 策略测试：internal/policy/local_first_test.go
- 模式定义：api/v1alpha1/common_types.go

### 10.3 执行命令

~~~bash
cd /mnt/d/vGPU/accelerator-control-plane

go test ./internal/policy \
  -run 'TestLocalFirstSelect' \
  -count=1 -v

echo "exit=$?"
~~~

### 10.4 重点观察的真实测试场景

终端中应出现以下子测试：

~~~text
local_wins_over_higher_priority_remote
Auto_falls_back_to_remote_when_local_capacity_is_short
LocalOnly_never_selects_remote_when_local_capacity_is_short
RemoteOnly_ignores_an_eligible_local_candidate
unschedulable_when_capacity_is_short
resource_form_and_profile_prevent_whole_device_and_MIG_units_from_mixing
~~~

### 10.5 预期结论

| 子场景 | 预期 |
|---|---|
| 本地与模拟远端同时可用 | Auto 选择 Local |
| 本地容量不足、模拟远端足够 | Auto 选择 Remote |
| LocalOnly 本地不足 | Unschedulable，不越权选择远端 |
| RemoteOnly 同时存在本地 | 忽略本地，选择 Remote |
| 全部容量不足 | Unschedulable |
| 请求 MIG profile | 不会与 WholeDevice 混用 |

> 【现场截图位置 P-01：三种模式策略实验】
>
> 截图目的：让读者直接看到 LocalOnly、RemoteOnly、Auto 和容量不足场景全部通过。
>
> 必须包含：调用端 WSL 提示符、仓库路径、完整 go test 命令、上述关键子测试名、PASS 和 exit=0。
>
> 建议文件名：docs/images/platform-adapter/P-01-placement-modes.png
>
> 建议图注：图 P-01　平台在仅本地、仅远端和自动选择模式下的策略模拟结果。

这个实验只证明选择逻辑，不证明模拟远端端点能够执行计算。

## 11. 模拟实验二：本地资源盘点与容量正确性

### 11.1 实验目的

验证平台不会把不同形态的本地加速资源混为一谈，并能正确扣除 Pod 和 Lease 占用。

实验关注：

- WholeDevice、MIG、Shared 被归一化为不同 Candidate；
- 已绑定 Pod 和未绑定 Lease 不会被重复计算；
- 共享同一资源名的不同 Pool 能看到一致的真实占用；
- 新 Lease 在 status 初始化前已经消耗容量；
- Released Lease 不再占用资源；
- Pod 的 nodeSelector、affinity、taint/toleration 仍被尊重。

### 11.2 对应实现

- 资源读取：internal/inventory/reader.go
- 容量核算：internal/inventory/accounting.go
- 测试：internal/inventory/reader_test.go
- 测试：internal/inventory/accounting_test.go
- 测试：internal/inventory/capacity_test.go

### 11.3 执行命令

~~~bash
cd /mnt/d/vGPU/accelerator-control-plane

go test ./internal/inventory \
  -run 'TestKubernetesReaderNormalizesWholeMIGAndSharedLocalResources|TestLocalAccountingAvoidsBoundLeaseDoubleCount|TestNewLeaseConsumesCapacityBeforeStatusInitialization|TestNodeMatchesPodSchedulingConstraints|TestSummarizeLocalPoolCapacityIncludesPodsAndUnboundLeases' \
  -count=1 -v

echo "exit=$?"
~~~

### 11.4 预期结论

所有测试通过说明：

1. 平台知道“1 张整卡”“1 个 MIG 实例”“1 个软件共享份额”不是同一种容量；
2. 平台不会因为 Pod 和 Lease 同时存在而重复扣减；
3. 刚创建的 Lease 不会留下可被并发任务利用的空窗；
4. 资源选择不会绕过原 Pod 的硬调度约束。

> 【现场截图位置 P-02：资源盘点与容量核算】
>
> 必须包含：完整命令、Whole/MIG/Shared 归一化测试、避免重复计数测试、新 Lease 即时占用测试、调度约束测试、PASS 和 exit=0。
>
> 建议文件名：docs/images/platform-adapter/P-02-inventory-accounting.png
>
> 建议图注：图 P-02　本地异构资源归一化与容量核算模拟验证。

## 12. 模拟实验三：一次本地任务的控制面闭环

### 12.1 实验目的

用 fake Kubernetes client 模拟一个完整本地任务：

~~~text
创建 Workload
  -> 发现 node-a 有 1 个 nvidia.com/gpu
  -> 选择 Local
  -> 创建 Lease
  -> Reserve
  -> 生成本地 RuntimePlan
  -> 生成 Pod API 对象
  -> 模拟 Pod Ready
  -> 模拟 Pod Succeeded
  -> Release Lease
~~~

这里的 Pod 只是内存中的 Kubernetes API 对象，不会拉取 example.test/workload:latest，也不会真正启动容器。

### 12.2 对应实现

- 生命周期测试：internal/controller/workload_controller_test.go 中的 TestWorkloadReconcilerLocalLifecycle
- 控制器主链：internal/controller/workload_controller.go
- 本地 Provider：internal/provider/local/provider.go
- 本地 Allocator：internal/allocation/local.go
- Pod 计划写入：internal/injector/pod.go

### 12.3 执行命令

~~~bash
cd /mnt/d/vGPU/accelerator-control-plane

go test ./internal/controller \
  -run '^TestWorkloadReconcilerLocalLifecycle$' \
  -count=1 -v

echo "exit=$?"
~~~

### 12.4 测试内部检查

该测试不是只检查函数没有报错，还会确认：

- Workload 到达 Starting；
- Workload 保存 PodRef；
- 生成的 Pod 对 worker 容器注入 1 个 nvidia.com/gpu；
- Workload 和 Lease 的 RuntimeReady condition 为 True；
- 模拟 Pod Ready 后 Workload 进入 Running；
- 模拟 Pod Succeeded 后 Workload 进入 Succeeded；
- Lease 最终进入 Released。

> 【现场截图位置 P-03：本地任务生命周期】
>
> 必须包含：测试命令、TestWorkloadReconcilerLocalLifecycle、PASS 和 exit=0。截图旁的正文应列出测试内部检查，不要把 fake Pod 写成真实容器。
>
> 建议文件名：docs/images/platform-adapter/P-03-local-lifecycle.png
>
> 建议图注：图 P-03　本地任务从资源选择、租约预留到释放的控制面闭环测试。

## 13. 模拟实验四：两个任务竞争一个排他端点

### 13.1 实验目的

验证最容易出错的并发场景：两个任务同时认为同一个模拟远端端点可用。

预期顺序：

~~~text
任务 A 首次 Claim endpoint-a  -> 成功
任务 A 重复 Claim             -> 成功，说明幂等
任务 B Claim endpoint-a       -> Conflict
任务 B 释放 endpoint-a        -> Conflict，非所有者不能释放
任务 A 释放 endpoint-a        -> 成功
最终 Claim 对象不存在          -> 容量已归还
~~~

### 13.2 对应实现

- 原子 Claim：internal/allocation/kubernetes_claims.go
- 模拟测试：internal/allocation/kubernetes_claims_test.go

### 13.3 执行命令

~~~bash
cd /mnt/d/vGPU/accelerator-control-plane

go test ./internal/allocation \
  -run 'TestKubernetesEndpointClaimerIsAtomicAndIdempotent|TestKubernetesEndpointClaimerReclaimsOrphanedClaim' \
  -count=1 -v

echo "exit=$?"
~~~

### 13.4 预期结论

通过结果证明：

- 同一所有者重复操作不会产生重复占用；
- 不同所有者不能同时持有一个排他端点；
- 非所有者不能释放别人的资源；
- 所有者释放后，端点可以再次使用；
- 如果旧 Claim 的业务 Lease 已不存在，控制面可以回收孤儿占用。

这仍然只证明 Kubernetes 控制面中的所有权，不证明真实远端 GPU 已经实现物理隔离。

> 【现场截图位置 P-04：排他端点竞争与释放】
>
> 必须包含：两个 Claim 测试名、PASS 和 exit=0。正文或终端说明应明确 endpoint-a 是模拟端点。
>
> 建议文件名：docs/images/platform-adapter/P-04-endpoint-claim.png
>
> 建议图注：图 P-04　两个任务竞争同一排他端点时的原子占用、幂等与释放验证。

## 14. 模拟实验五：控制器重复执行与恢复

### 14.1 实验目的

验证 Controller 被重复触发时不会创建重复对象，并且已有状态能够成为恢复依据。

本实验选择不依赖真实远端执行的本地恢复场景：预先创建一个确定名称的 Pod API 对象，再让 Controller 重复 Reconcile。

### 14.2 对应实现

- 恢复测试：internal/controller/workload_controller_additional_test.go 中的 TestWorkloadReconcilerRestoresAnExistingPodIdempotently
- 确定性子对象命名：internal/controller/names.go
- 状态恢复：internal/controller/workload_controller.go 中的 restoreLeaseSelection 和 ensurePod

### 14.3 执行命令

~~~bash
cd /mnt/d/vGPU/accelerator-control-plane

go test ./internal/controller \
  -run '^TestWorkloadReconcilerRestoresAnExistingPodIdempotently$' \
  -count=1 -v

echo "exit=$?"
~~~

### 14.4 预期结论

测试会确认：

- 已有 Lease 能被重新读取；
- 已有 Pod 能被重新关联；
- 连续多次 Reconcile 后仍然只有 1 个 Pod；
- Workload 能恢复到 Starting；
- 重试不会重新选择另一个目标。

> 【现场截图位置 P-05：幂等与恢复】
>
> 必须包含：恢复测试名、PASS 和 exit=0。截图说明中应强调“重复调谐后 Pod API 对象数量仍为 1”。
>
> 建议文件名：docs/images/platform-adapter/P-05-idempotent-restore.png
>
> 建议图注：图 P-05　Controller 重复调谐和已有对象恢复的幂等性验证。

## 15. 如何判断平台模拟验证是否通过

不能只看一个 PASS。完整结论应由下列证据共同组成：

| 证据 | 回答的问题 | 对应实验 |
|---|---|---|
| LocalOnly/RemoteOnly/Auto | 用户意图是否被严格遵守 | P-01 |
| Whole/MIG/Shared 和容量扣减 | 资源盘点是否准确 | P-02 |
| Decision、Lease、RuntimePlan、Released | 生命周期是否闭环 | P-03 |
| 原子 Claim | 并发任务是否会重复占用 | P-04 |
| 重复 Reconcile 后无重复 Pod | 控制器是否幂等、可恢复 | P-05 |

### 15.1 当前实验结论应怎样表述

推荐表述：

> 平台适配层已通过场景化模拟实验验证统一资源建模、本地/模拟远端选择、本地优先策略、容量核算、租约所有权、排他端点冲突、控制器幂等和本地控制面生命周期。实验未启动真实业务容器，也未连接真实远端 GPU 运行时。

不推荐表述：

> 平台已经实现本地与远端 GPU 无感运行切换。

后一句超出了当前证据范围。

### 15.2 截图清单

| 编号 | 展示内容 | 状态 |
|---|---|---|
| P-01 | LocalOnly、RemoteOnly、Auto 策略场景 | 待采集真实终端截图 |
| P-02 | 本地异构资源盘点与容量核算 | 待采集真实终端截图 |
| P-03 | 本地 Workload 到 Lease Released 的闭环 | 待采集真实终端截图 |
| P-04 | 两个任务竞争一个模拟排他端点 | 待采集真实终端截图 |
| P-05 | Controller 重复调谐和已有对象恢复 | 待采集真实终端截图 |

截图中应同时保留：

- WSL 用户和主机名；
- 仓库路径；
- 完整或可辨识的命令；
- 测试场景名；
- PASS；
- exit=0。

### 15.3 2026 年 8 月 20 日本轮重新验证记录

本轮在本地 Windows 主机的 Ubuntu WSL 中，严格按照第 10～14 章列出的测试选择条件依次执行。五组测试串行完成，最终进程退出码为 0。

| 实验 | 本轮结果 | 实际通过的关键测试 |
|---|---|---|
| P-01 三种模式策略 | PASS | TestLocalFirstSelect 及其 LocalOnly、RemoteOnly、Auto、容量不足、MIG profile 子场景 |
| P-02 资源盘点与容量 | PASS | Whole/MIG/Shared 归一化、避免重复计数、新 Lease 即时占用、Pod 调度约束、本地容量摘要 |
| P-03 本地生命周期 | PASS | TestWorkloadReconcilerLocalLifecycle |
| P-04 排他端点竞争 | PASS | TestKubernetesEndpointClaimerIsAtomicAndIdempotent、TestKubernetesEndpointClaimerReclaimsOrphanedClaim |
| P-05 幂等恢复 | PASS | TestWorkloadReconcilerRestoresAnExistingPodIdempotently |

本轮验证只读取和执行现有 Go 测试，没有启动真实容器、没有访问真实 GPU、没有连接真实远端计算服务，也没有修改平台代码。该记录可作为后续采集 P-01～P-05 真实终端截图时的命令基线。

## 16. 后续接入真实执行能力的计划

当前平台已经能够形成 Decision、Lease 和 RuntimePlan，但要让业务真正运行，还需要明确的数据面执行接口。

### 16.1 第一步：固定控制面与执行面的边界

建议把以下状态作为稳定交接点：

~~~text
Decision 已保存
Lease 已预留
RuntimePlan 已生成
~~~

执行器只消费 RuntimePlan，不重新选择资源。

### 16.2 第二步：定义 Executor

Executor 可以理解为“执行器”，建议至少包含：

~~~text
Prepare  -> 准备运行环境
Start    -> 启动任务
Observe  -> 查询运行状态
Stop     -> 停止任务
Cleanup  -> 清理运行资源
~~~

未来实现可以是：

- Kubernetes Pod Executor；
- 本地主机进程 Executor；
- WSL 进程 Executor；
- 外部作业系统 Executor。

### 16.3 第三步：分别接入本地与远端数据面

本地执行需要验证：

- Device Plugin 资源真实存在；
- Pod 成功调度；
- 容器能看到正确设备；
- GPU 计算结果通过 CPU 对照；
- 失败和退出码能回传；
- Lease 能随任务结束释放。

远端执行需要另外定义：

- Endpoint 如何注册和探活；
- 客户端运行环境怎样交付；
- 地址、认证和加密怎样配置；
- 排他和共享容量怎样对应真实设备；
- 连接错误怎样映射为平台状态；
- 日志和计算结果怎样回传；
- 清理失败怎样恢复。

具体选择哪一种远端技术、是否使用容器、运行环境怎样匹配，目前应作为后续设计和验收任务，不写成本文已经完成的能力。

### 16.4 第四步：组合验收

只有同时满足以下条件，才可以正式声明平台完成真实本地/远端执行接入：

~~~text
平台选择结果正确
  + Lease 所有权正确
  + RuntimePlan 与选择一致
  + Executor 启动成功
  + 真实计算结果正确
  + 故障能够归因
  + 删除后容量释放
  + Controller 重启后能够恢复
~~~

## 17. 结论

accelerator-control-plane 当前最重要的成果不是某个具体 GPU 调用工具，而是一条清晰的异构资源控制链：

~~~text
统一任务意图
  -> 统一资源候选
  -> 本地/模拟远端模式选择
  -> 可解释 Decision
  -> 可恢复 Lease
  -> RuntimePlan
  -> 生命周期与释放
~~~

这条控制链使业务不必在提交入口直接绑定某个节点或某种远端技术，也为后续增加真实 Executor 和远端 Runtime Adapter 留出了稳定边界。

本文的模拟实验分别验证了选择、盘点、生命周期、并发占用和恢复五个关键问题。它们足以证明控制面设计具备一致性和可扩展基础，但不会被夸大为真实容器执行或真实远端 GPU 调用已经完成。
