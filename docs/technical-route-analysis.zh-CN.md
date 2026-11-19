# 本地与远端 GPU 透明调用技术路线调研

## 1\. 问题

1. GPU 在当前 Kubernetes 节点或集群内时，怎样发现、分配、切分和调度？

2. GPU 不在工作负载所在机器时，怎样让应用继续调用 CUDA/NVML，而不把业务改成 RPC 客户端？

3. 平台怎样在任务开始前决定使用哪一条路径，并把对应的运行环境放进容器？

4. 仓库是否开源、能解决流程中的哪一步、哪些关键能力仍不由它们提供？

本文中的无感指应用不需要填写 GPU 的物理位置，并且在已选择的本地或远端运行环境中使用既有 CUDA 接口。它**不等于**进程运行到一半可以把 CUDA 上下文自动搬到另一张 GPU。

## 2\. 技术路线

### 2\.1 两个场景

```Plaintext
场景一：工作负载所在节点没有可用本地 GPU

业务容器 ── CUDA/NVML 调用 ──> 远端 runtime client ── 网络 ──> GPU Server ──> 真实 GPU


场景二：工作负载所在节点或集群存在本地 GPU

业务容器 ── CUDA/NVML 调用 ──> 本地 NVIDIA Driver ──> 本地 GPU
      │
      └── 启动前由平台决定：本次走本地路径，还是走远端路径
```

两种场景的业务代码可以相同，但容器内的运行时、资源申请和设备访问路径不同。因而本地和远端不是同一个设备插件可以同时解决的问题：

- **本地路径**的核心是驱动、Container Runtime、Device Plugin、GPU 切分和 Kubernetes 调度；

- **远端路径**的核心是 CUDA/NVML API 拦截、参数/内存/RPC 转发、远端 server 和网络连接；

- **切换路径**的核心是在容器启动前选择一条路径，并使一次运行保持绑定到该路径。

### 2\.2 整体流程

```Plaintext
工作负载提交
      │
      ▼
[1] 查询本地 GPU 是否满足请求
      │
      ├── 满足 ──> [2A] 本地 GPU 分配与原生 CUDA 运行
      │
      └── 不满足/策略要求远端 ──> [2B] 选择远端 endpoint
                                            │
                                            ▼
                                   [3] 注入远端 CUDA runtime
                                            │
                                            ▼
                                   [4] CUDA 调用经网络到 GPU Server

在 [1] 与 [2A]/[2B] 之间：完成一次本地/远端模式选择和资源预留
```

### 2\.3 统一的工作负载流转

|流程步骤|本地 GPU 路径|远端 GPU 路径|需要回答的调研问题|
|---|---|---|---|
|提交请求|Pod 声明 GPU 数量、型号/显存/切分需求|Pod 不直接写远端地址，而是声明同样的逻辑 GPU 需求|请求如何表达本地优先、远端优先或只能远端？|
|查询候选|读取节点设备、切分实例、调度队列|读取远端 host、GPU、runtime 版本、网络可达性|如何比较“本地一张卡”与“远端一个 endpoint”？|
|绑定资源|Device Plugin/HAMi 分配设备或实例|分配远端 endpoint 与会话|如何防止两个任务同时拿到同一远端 GPU？|
|物化运行环境|容器获得 `/dev/nvidia*`、驱动库和可见设备|容器获得 shim、endpoint、认证材料和环境变量|如何保证应用无需改启动命令？|
|调用 GPU|标准 CUDA 进入本机驱动|CUDA/NVML 被 shim 拦截后发至 server|runtime 覆盖哪些 API、内存和多卡语义？|
|结束与释放|kubelet/Device Plugin 释放本地设备|client/server 结束会话，回收远端 GPU|如何让释放和异常清理可重复执行？|

## 3\. 本地 GPU 使用路线：无感切换中的本地分支

本地路线不需要 GPU\-over\-IP。工作负载最终使用本机的 NVIDIA 驱动和真实设备；差异仅在于整卡分配、硬件切分、软件切分和队列调度。在本地/远端无感切换中，它们都是本地模式的候选实现：控制器选中本地后，只注入这里的本地资源请求，不加载任何远端 shim。

### 3\.1 路线 L1：GPU Operator \+ NVIDIA Device Plugin 的原生整卡路径

```Plaintext
GPU 节点
  ├── NVIDIA Driver
  ├── NVIDIA Container Runtime
  ├── NVIDIA GPU Operator
  └── NVIDIA k8s-device-plugin
          │
          ▼
     kubelet 暴露 nvidia.com/gpu
          │
          ▼
     Pod limits: nvidia.com/gpu: 1
          │
          ▼
     容器使用本地 CUDA 与本地物理 GPU
```

这是最标准、最容易理解的本地路线：Pod 请求 `nvidia.com/gpu`，Kubernetes 将其调度到有可用 GPU 的节点，Device Plugin 在分配阶段将设备访问信息交给容器运行时。

#### 仓库分析：NVIDIA GPU Operator

|项目|分析|
|---|---|
|仓库|[NVIDIA/gpu\-operator](https://github.com/NVIDIA/gpu-operator)|
|是否开源|是，Apache\-2\.0|
|在流程中的位置|GPU 节点初始化与组件生命周期；位于本地路径的基础设施侧|
|代码/部署形态|Kubernetes Operator；管理 Driver、NVIDIA Container Runtime、Device Plugin、节点标签和 DCGM 监控等组件|
|可以直接复用|将裸 GPU 节点标准化为 Kubernetes 可识别、可运行 CUDA 容器、可监控的节点|
|不负责什么|不转发远端 CUDA 调用；不把远端 GPU 注册为本地设备；不判断某个业务应选本地还是远端|
|适合的路线|NVIDIA 本地设备底座|

#### 仓库分析：NVIDIA k8s\-device\-plugin

|项目|分析|
|---|---|
|仓库|[NVIDIA/k8s\-device\-plugin](https://github.com/NVIDIA/k8s-device-plugin)|
|是否开源|是，Apache\-2\.0|
|在流程中的位置|本地设备发现、健康上报和容器设备分配；直接位于 `[2A]`|
|核心机制|以 DaemonSet 运行，通过 Kubernetes Device Plugin API 向 kubelet 上报 GPU 数量和健康状态；Pod 请求 `nvidia.com/gpu` 时参与 Allocate|
|可以直接复用|整卡 GPU 的 Kubernetes 标准暴露和分配路径|
|不负责什么|资源切分策略、队列公平、远端 GPU 端点、跨资源池预留、远端 runtime 注入|

L1 的特点是简单、稳定、兼容性最好。它适用于业务确实在有 GPU 的节点运行，且只需要整卡分配的情况。

### 3\.2 路线 L2：HAMi 的本地 GPU 共享、切分与 MIG 路径

```Plaintext
Pod 提交
  │
  ▼
HAMi mutating webhook
  │
  ▼
HAMi scheduler filter / score / bind
  │
  ▼
HAMi device plugin 与容器内虚拟化组件
  │
  ▼
整卡 / 显存与算力份额 / 动态 MIG 实例
```

当本地 GPU 不希望只能“整卡给一个 Pod”时，可以引入 HAMi。它的重点是让设备份额、显存和算力约束成为调度对象，并在容器内落实对应的可见设备与限制。

#### 仓库分析：HAMi

|项目|分析|
|---|---|
|仓库|[Project\-HAMi/HAMi](https://github.com/Project-HAMi/HAMi)|
|是否开源|是，Apache\-2\.0；CNCF Incubating|
|在流程中的位置|本地资源发现、请求改写、设备感知调度、分配和容器内资源虚拟化|
|主要组成|mutating webhook、scheduler extender、device plugin、容器内虚拟化组件和监控组件|
|可以直接复用|GPU 共享、显存/算力限制、设备感知调度、多种加速设备后端、动态 MIG（取决于具体硬件与模式）|
|不负责什么|远端 CUDA API 传输、远端 GPU Server、远端端点会话、跨网络 GPU 内存语义|
|与 GPU Operator 的关系|可与 GPU Operator 共存：Operator 负责 NVIDIA 软件栈，HAMi 侧重共享、切分与调度|
|适合的路线|需要本地分数卡、MIG、共享 GPU、多租户资源隔离的 L2|

HAMi 的“GPU 共享”与远端 GPU\-over\-IP 是两类问题。前者是在本地节点对一张物理 GPU 进行切分或共享，后者是把另一台机器上的 CUDA 调用通过网络转发。报告中应分别描述，避免出现“用了 HAMi 就能用远端 GPU”的误读。

#### L2 中的三种本地资源形态

|资源形态|含义|对应用的表现|
|---|---|---|
|整卡|一个工作负载独占一张物理 GPU|普通 `CUDA_VISIBLE_DEVICES` 设备|
|MIG 实例|GPU 硬件划分出的固定 profile 实例|容器看到一个隔离的 MIG 设备|
|软件份额|按显存、算力或时间片共享一张 GPU|容器仍使用可见 GPU，但资源上限由中间件控制|

### 3\.3 路线 L3：在本地路径上叠加队列、批调度和公平调度

L1/L2 解决的是“GPU 如何作为设备分配给容器”。训练、HPC 或多团队共享集群还可能需要队列、gang scheduling、拓扑感知和公平性策略。此时可选择 Volcano 或 KAI Scheduler 作为本地路径的调度增强组件。

#### 仓库分析：Volcano

|项目|分析|
|---|---|
|仓库|[volcano\-sh/volcano](https://github.com/volcano-sh/volcano)|
|是否开源|是，Apache\-2\.0；CNCF Incubating|
|在流程中的位置|Pod/Job 的批调度、队列、gang scheduling 与资源治理|
|主要能力|Kubernetes 原生批调度；适合 AI、ML、HPC 等成组或弹性任务；与 PyTorch、TensorFlow、Ray、MPI 等生态集成|
|可以直接复用|队列、作业组同时调度、批处理排队、拓扑相关调度能力|
|不负责什么|它不会把 CUDA API 转发到远端 GPU，也不会自己生成远端 runtime client|
|适合的路线|需要批训练、分布式任务和队列管理的 L1/L2 扩展|

#### 仓库分析：KAI Scheduler

|项目|分析|
|---|---|
|仓库|[kai\-scheduler/KAI\-Scheduler](https://github.com/kai-scheduler/KAI-Scheduler)|
|是否开源|是，Apache\-2\.0；CNCF Sandbox|
|在流程中的位置|AI 工作负载的 GPU 调度、队列、公平性、资源预留和拓扑感知|
|主要能力|batch scheduling、队列层级、DRF/公平性、binpack/spread、GPU sharing、DRA 和拓扑感知调度|
|可以直接复用|多团队 GPU 队列治理、GPU 调度策略和规模化 AI 调度能力|
|不负责什么|不提供远端 CUDA/NVML shim；无法仅靠调度器让远端 GPU 对应用透明|
|适合的路线|需要比默认 kube\-scheduler 更强 GPU 队列/公平调度的 L1/L2 扩展|

### 3\.4 本地路线小结

|路线|主要组合|GPU 分配形态|能解决的事|不解决的事|
|---|---|---|---|---|
|L1 原生整卡|GPU Operator \+ Device Plugin|整卡|节点初始化、设备发现、标准 Pod GPU 分配|分数卡、远端调用、跨池选择|
|L2 本地切分|L1 \+ HAMi|整卡、MIG、软件份额|共享、切分、设备感知分配|远端 CUDA 调用|
|L3 调度增强|L1/L2 \+ Volcano 或 KAI|取决于底层设备方案|队列、gang、公平性、拓扑策略|远端 runtime 与远端 session|

## 4\. 远端 GPU 调用路线

远端路径的目标是：应用仍以为自己在调用 CUDA/NVML，实际由 client runtime 截获调用，经网络发送给远端 GPU Server，再由后者调用真实 NVIDIA 驱动。

### 4\.1 远端调用的共同结构

```Plaintext
应用 / PyTorch / CUDA 程序
        │
        │ cuInit / cuMemAlloc / kernel launch / NVML 查询 ...
        ▼
Client runtime shim 或 LD_PRELOAD 库
        │
        │ 序列化 API 参数、句柄、内存操作、返回值
        ▼
网络协议（TCP / HTTP2 / RPC 等）
        │
        ▼
远端 GPU Server
        │
        ▼
真实 NVIDIA Driver + 真实 GPU
```

远端 runtime 是否可用，取决于它对目标 CUDA API、CUDA Runtime/Driver API、NVML、库调用、内存模型、线程模型和框架构建方式的覆盖。`nvidia-smi` 能通过，不表示目标 PyTorch、vLLM 或自定义 CUDA 扩展都能通过。

### 4\.2 路线 R1：Lupine——基于 CUDA/NVML shim 的 GPU\-over\-IP

```Plaintext
GPU Host                              Client Pod / CPU Node
────────                              ────────────────────
lupine-server                         lupine client image
真实 NVIDIA GPU                       /opt/lupine/lib/libcuda.so.1
                                      /opt/lupine/lib/libnvidia-ml.so.1
        <──────────── HTTP/2 / 网络 RPC ────────────>
```

#### 仓库分析：Lupine

|项目|分析|
|---|---|
|仓库|[lupinemachines/lupine](https://github.com/lupinemachines/lupine)|
|是否开源|是，Apache\-2\.0|
|开源完整性|client shim、server、codegen、Dockerfile、测试与发布镜像均在公开仓库中；适合以固定版本构建/引用|
|在流程中的位置|远端路径的 `[3]` runtime 注入与 `[4]` CUDA 调用转发|
|client 机制|使用 `libcuda.so.1` shim 和 `libnvidia-ml.so.1` shim；发布的 client image 已设置对应库路径，也可通过 `LD_PRELOAD` 用于开发验证|
|server 机制|GPU host 运行 server，server 使用本机真实 GPU；公开镜像按 CUDA/Ubuntu 标签提供|
|API 生成方式|codegen 读取 CUDA、cuBLAS、cuDNN、NVML 等头文件生成 RPC 包装；扩展 API 需要重新生成并做语义验证|
|多 GPU/多 server|client 可配置多个 server，按 server 顺序暴露成设备序列；这只是设备枚举聚合，不是调度、配额或会话管理|
|网络与安全|支持放在 TLS 终止代理之后；认证和多租户授权仍需要外部系统提供|
|已明确的调用边界|跨 server 的 D2D/Peer copy 会经 client 中转；直接 server\-to\-server 传输、跨 server peer access 等能力并非全部实现|
|能直接复用|远端 CUDA/NVML 的基础透明调用、容器化 client/server、版本化镜像、单机/多 server 演示路径|
|仍需外部补充|endpoint 库存、会话分配、租约、健康管理、审计、身份认证、Kubernetes 资源模型和本地/远端选择|

Lupine 解决已经决定走远端后，容器如何调用 GPU，不解决应不应该走远端、哪张卡属于哪个任务。

### 4\.3 路线 R2：Juice——GPU\-over\-IP 的 client/server 产品形态

```Plaintext
应用程序
   │
   └── Juice client runtime
             │
             └── 标准 TCP/IP ──> Juice server ──> 物理 GPU
```

#### 仓库分析：Juice Community Version

|项目|分析|
|---|---|
|仓库|[Juice\-Labs/Juice\-Labs](https://github.com/Juice-Labs/Juice-Labs)|
|是否开源|是，社区仓库标为 MIT；仓库同时包含 `TERMS.md`，实际采用时应同时审查许可证和条款|
|发布状态|GitHub 页面显示的公开最新 release 为 2023\-08；应固定具体版本并复测目标环境，不应假定其始终跟随最新 CUDA/Kubernetes|
|在流程中的位置|远端路径的 runtime client、GPU Server、部署与管理工具|
|client/server 机制|GPU Server 运行在有物理 GPU 的机器上，client 运行在消费端；项目说明客户端应用无需修改，可部署于物理机、VM 和容器|
|跨平台特征|项目说明 Linux 和 Windows 均可作为 client/server 环境，这是其与部分 Linux\-only runtime 的差异点|
|可以直接复用|GPU\-over\-IP 基本链路、部分 CLI/controller/chart 组织方式，以及跨平台部署经验|
|不自动提供|与 Kubernetes 本地资源的统一选址、跨项目租约一致性、应用级兼容性承诺、平台侧审计模型|
|适合的路线|需要对比不同 GPU\-over\-IP 运行时，或需要评估 Windows/VM/容器混合环境时的远端调用路线|

Juice 的仓库具有较完整的产品化外壳，适合研究远端 runtime 的部署、管理和跨环境运行方式。与 Lupine 一样，它不能代替本地/远端选择控制器；两者的差别应通过目标应用兼容性和运维要求比较，而不是只看“是否能远端调用 GPU”。

### 4\.4 路线 R3：Cricket——基于 RPC 的远端 CUDA 调用路线

```Plaintext
动态链接 CUDA 应用
      │
      ▼
cricket-client.so
      │
      ▼
RPC / rpcbind 服务发现
      │
      ▼
cricket-rpc-server
      │
      ▼
远端 GPU 与 CUDA Driver
```

#### 仓库分析：Cricket

|项目|分析|
|---|---|
|仓库|[RWTH\-ACS/cricket](https://github.com/RWTH-ACS/cricket)|
|是否开源|是，MIT；第三方依赖应在实际镜像中单独生成许可证清单|
|在流程中的位置|远端路径的 client 拦截库和 RPC server|
|client 机制|运行时以 `cricket-client.so` 形式进入应用进程；应用需要走可拦截的动态 CUDA 调用路径|
|server 机制|远端使用 `cricket-rpc-server`，经 RPC 服务号/版本发现服务；常见部署需要处理 `rpcbind` 和服务端口可达性|
|框架边界|目标程序的 CUDA 链接方式会直接影响可用性；某些框架路径需要特定构建方式，不能假定所有 PyTorch 环境都能直接工作|
|可以直接复用|RPC 型远端 CUDA 调用链路，以及“每个 GPU server/可见设备组对应独立 endpoint”的部署思路|
|不自带的能力|通用 TLS/认证、端点库存、独占会话、自动切分、稳定固定端口的 Kubernetes Service 建模|
|适合的路线|API 范围受控、网络与部署受控的远端 CUDA 方案；也可作为 Lupine/Juice 的实现对照|

Cricket 的路线更接近传统 RPC 型远端 CUDA 中间件。它适合把RPC 服务发现、client preload、server 可见设备绑定作为独立技术路线分析，但不宜将其当成不需要附加控制组件的完整资源池方案。

### 4\.5 路线 R4：SCUDA——通过 CUDA 头文件 codegen 生成 RPC 包装

#### 仓库分析：SCUDA GPU\-over\-IP bridge

|项目|分析|
|---|---|
|仓库|[vn\-os/scuda\_GPUs\-over\-IP\-bridge](https://github.com/vn-os/scuda_GPUs-over-IP-bridge)|
|是否开源|是，Apache\-2\.0；GitHub 标记为 Lupine fork|
|在流程中的位置|远端 CUDA API 拦截和 RPC 生成|
|核心实现|从 CUDA、cuBLAS、cuDNN、NVML 等头文件读取声明，生成 client/server 侧包装代码；client 使用 `LD_PRELOAD` 加载 `libscuda_*`|
|可以直接复用|“如何从 CUDA API 声明生成 RPC stub”的实现思路，以及 CUDA 版本相关库的构建路径|
|与 Lupine 的关系|两者技术族高度接近；SCUDA 本身是 fork，不宜在无特定功能差异的情况下与上游同时作为生产基础|
|不负责什么|调度、容量、租约、远端访问认证和本地/远端切换|
|适合的路线|研究 CUDA API 覆盖、codegen 机制或定制 RPC wrapper 时的参考实现|

### 4\.6 路线 R5：mrCUDA——远端到本地迁移的研究原型

#### 仓库分析：mrCUDA

|项目|分析|
|---|---|
|仓库|[EBD\-CREST/mrCUDA](https://github.com/EBD-CREST/mrCUDA)|
|是否开源|是，GPL\-3\.0|
|研究目标|在 rCUDA 基础上研究 remote\-to\-local GPU execution migration，即远端运行后本地 GPU 可用时的迁移|
|依赖环境|README 仍列出 CUDA 7\.0、Python 2\.7、rCUDA v15\.07 等前提|
|可以提供的价值|说明“运行中从远端迁到本地”不是启动前选址，而是 CUDA 执行状态迁移问题；可作为论文、状态处理和实验设计参考|
|不宜直接复用的原因|依赖栈过旧，GPL\-3\.0 需要单独处理，且不是面向当代 CUDA/框架/Kubernetes 的生产组件|
|适合的路线|仅作为运行中迁移的研究资料，不作为远端 GPU 调用的工程底座|

## 5\. 本地/远端无感切换路线

无感切换不需要改变应用的算法或 CUDA 调用，它需要改变的是**容器创建前的资源绑定和运行时装配**。一次任务被选为本地模式后，就用第 3 节中的本地原生 runtime；被选为远端模式后，就加载第 4 节中的一个远端 shim 并固定连接到已分配的 endpoint。

一条完整路线必须由三个彼此独立的部件组成：

|部件|可选实现|在切换中的职责|
|---|---|---|
|本地 GPU 路线|原生整卡 Device Plugin；HAMi/静态 MIG；叠加 Volcano/KAI 调度|本地模式下怎样让容器获得 GPU|
|远端 GPU 路线|Lupine；Juice；Cricket；SCUDA|远端模式下怎样把 CUDA/NVML 调用发到 GPU Server|
|启动前选择器|Admission Webhook；Workload Controller；CRD/DRA|选择本次运行采用本地还是远端，并装配对应 Pod|

同一个业务 Pod 一次只能落在一种运行模式：本地模式不加载远端 shim；远端模式不申请本地 GPU。两种模式的 CUDA runtime 不能在同一个业务容器中并存。

### 5\.1 启动前模式选择的整体流程

```Plaintext
提交逻辑 GPU 请求
      │
      ▼
读取本地候选：节点 GPU / MIG / 软件份额 / 队列状态
      │
读取远端候选：GPU Host / endpoint / runtime 版本 / 可用会话
      │
      ▼
兼容性过滤
  - GPU 型号、显存、切分类型
  - CUDA Driver/runtime 版本
  - 应用框架与远端 runtime 兼容性
  - 网络域与访问权限
      │
      ▼
选择本地模式或远端模式，并预留对应资源
      │
      ├── 本地模式：写入本地 GPU 资源请求，走 Device Plugin/HAMi
      │
      └── 远端模式：注入 shim、endpoint、会话凭据，连接 GPU Server
      │
      ▼
启动业务容器；本次运行期间不更换模式
```

远端的“endpoint”不应只是一条 IP 地址。至少应对应：GPU host、可见 GPU 或 GPU 组、runtime 类型和版本、会话标识、可用容量、健康状态和归属租户。否则多任务会将同一远端 GPU 当作无限资源。

### 5\.2 路线 S1：Admission Webhook 直接改写 Pod

```Plaintext
用户提交 Pod
   │
   ▼
Admission Webhook 查询本地/远端摘要
   │
   ├── 选择本地：保留/写入 nvidia.com/gpu 或 HAMi 请求
   │
   └── 选择远端：写入 runtime 挂载、环境变量、initContainer 或 sidecar 配置
   │
   ▼
Pod 被创建并由正常调度链路运行
```

|项目|分析|
|---|---|
|路线特点|在 Pod admission 阶段一次性选择模式，并将本地或远端运行时配置直接写进 Pod|
|可达到的效果|业务只提交逻辑 GPU 请求；后续容器拿到本地设备或远端 shim；实现路径短|
|可利用的开源能力|Kubernetes Admission Webhook；GPU Operator/Device Plugin/HAMi 负责本地；Lupine/Juice/Cricket 负责远端 runtime|
|需要自定义的部分|候选查询、模式选择、远端 endpoint 预留、运行时注入模板|
|适用范围|单集群、规则较简单、先验证“启动前透明选择”的方案|
|流程特征|决策逻辑位于同步 admission 调用中，因此应只读取可快速获得的摘要状态；复杂排队与重试不适合塞在 webhook 内|

### 5\.3 路线 S2：先生成逻辑 Workload，再由 Controller 物化实际 Pod

```Plaintext
用户提交 AcceleratorWorkload（逻辑对象）
        │
        ▼
Controller 获取本地与远端完整候选
        │
        ▼
创建资源预留与绑定快照
        │
        ▼
Controller 生成一种实际 Pod
   ├── Local Pod：GPU Device Plugin/HAMi 配置
   └── Remote Pod：runtime shim + endpoint + 会话配置
```

|项目|分析|
|---|---|
|路线特点|把逻辑资源请求和实际 Pod 分开；Controller 先做选择和预留，再物化确定模式的 Pod|
|可达到的效果|可以记录完整的候选、选择理由、绑定快照和释放过程；可处理远端 endpoint 租约和异步状态|
|可利用的开源能力|Kubernetes CRD/Controller 机制；本地资源组件与远端 runtime 组件仍沿用前文仓库|
|需要自定义的部分|逻辑 Workload API、状态机、预留、租约、绑定快照、远端 Agent/会话接口|
|适用范围|多个远端 GPU host、需要可靠资源归属和可观察切换记录的方案|
|流程特征|实际 Pod 创建前已明确模式，因此 Pod 内不会同时带本地 GPU 与远端 shim 两套运行时|

### 5\.4 路线 S3：将远端 GPU 也作为 Kubernetes 可声明资源

```Plaintext
Pod / Job
   │ ResourceClaim / CRD
   ▼
逻辑 GPU 资源选择器
   │
   ├── 本地 GPU Device / MIG instance
   └── 远端 GPU endpoint / remote GPU group
   │
   ▼
资源绑定完成后再装配本地或远端 runtime
```

|项目|分析|
|---|---|
|路线特点|用 CRD 或 Kubernetes DRA 的 `ResourceClaim`/`DeviceClass` 描述本地设备和远端 endpoint；工作负载申请逻辑资源而非固定位置|
|可达到的效果|资源属性、配额、绑定关系和调度输入可以统一表达；适合多资源池、多团队和复杂设备约束|
|可利用的开源能力|Kubernetes [DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)、KAI Scheduler 的 DRA/调度能力、HAMi 的本地设备资源能力|
|需要自定义的部分|远端 GPU 的资源驱动或 CRD、远端会话分配、DRA 分配结果到 runtime 注入的映射|
|适用范围|远端 GPU 已具备可靠库存和会话模型，且需要将它纳入统一配额/调度语义的方案|
|需要澄清|DRA 解决的是资源声明和绑定；它并不实现 CUDA API 通过网络到远端 GPU，仍要与 Lupine/Juice/Cricket 等 runtime 组合|

### 5\.5 三种无感切换路线的区别

|路线|决策发生位置|实际 Pod 何时创建|远端资源预留|适合的场景|与远端 runtime 的关系|
|---|---|---|---|---|---|
|S1 Webhook 改写|Pod admission 时|admission 后立刻|需额外轻量接口|简单规则、快速验证|注入 Lupine/Juice/Cricket client 配置|
|S2 Controller 物化|逻辑 Workload reconcile 时|预留成功后|可由 Controller/Agent 管理|多 endpoint、需要可追踪绑定|以 Provider/模板注入具体 runtime|
|S3 CRD/DRA 统一资源|资源 claim/调度阶段|claim 绑定后|可由资源驱动表达|多池、配额、复杂资源属性|runtime 仍是独立的调用层|

### 5\.6 “启动前选择”与“运行中迁移”的分界

```Plaintext
启动前选择：                    运行中迁移：

还没有业务 CUDA 状态            已存在 CUDA context、显存、stream、event、句柄
        │                                      │
        ▼                                      ▼
选择本地或远端 runtime            必须复制/重建/切换运行态
        │                                      │
        ▼                                      ▼
启动一次固定模式的容器            需要处理 API、框架、通信和一致性语义
```

前文 S1/S2/S3 都属于“启动前选址与资源绑定”路线；它们不需要迁移已运行 CUDA context。mrCUDA 所代表的是另一类研究问题：远端执行已经开始后，如何迁到本地。两类问题在报告中应分成不同章节，不能把后者当作前者的自然副产品。

## 6\. 本地与远端的配对关系及完整路线组合

第 3、4、5 节分别讨论了本地、远端和选择器。本节把它们收束成可直接阅读的配对关系与完整组合；并非所有仓库都可以不加条件地拼在一起。

### 6\.1 本地路线与远端 runtime 的配对矩阵

|本地路线|远端仓库|能否配对|实际含义|
|---|---|---|---|
|原生整卡：GPU Operator \+ NVIDIA Device Plugin|Lupine|可以，资源语义最一致|本地 Pod 申请 `nvidia.com/gpu` 并使用原生 CUDA；远端 Pod 加载 Lupine `libcuda`/NVML shim|
|原生整卡：GPU Operator \+ NVIDIA Device Plugin|Juice|可以|本地使用原生 CUDA；远端 Pod 使用 Juice client，适合将跨平台 client/server 纳入比较|
|原生整卡：GPU Operator \+ NVIDIA Device Plugin|Cricket|可以，但运行环境受限|本地为标准 GPU Pod；远端走 Cricket RPC，目标应用须满足其 CUDA 动态链接与框架兼容条件|
|原生整卡：GPU Operator \+ NVIDIA Device Plugin|SCUDA|技术上可以|本地不受影响；远端使用 codegen/RPC shim。SCUDA 与 Lupine 路线接近，通常不需要并列部署两者|
|本地 HAMi 软件切分|Lupine / Juice|有条件可以|本地是资源份额，远端通常先是整卡；选择器必须区分隔离等级、显存和容量单位|
|本地静态 MIG|Lupine / Juice / Cricket|有条件可以|远端必须明确暴露匹配的 MIG 实例或独占 GPU，不能把两侧都笼统当作“1 GPU”|
|本地 HAMi/MIG \+ Volcano 或 KAI Scheduler|任一远端 runtime|不能直接配，需要选择器|Volcano/KAI 负责本地排队/调度，不发送 CUDA API；仍需 Controller、Webhook 或 DRA 接入远端 runtime|

从配对角度看，最简单的完整模式是：

```Plaintext
同一个逻辑 GPU 请求
        │
        ▼
启动前选择本地 或 远端
        │
  ┌─────┴─────┐
  ▼           ▼
本地原生      远端透明调用
GPU runtime   GPU-over-IP runtime
```

例如选择“原生整卡 Device Plugin \+ Lupine”时：

```Plaintext
本地选中：Pod -> nvidia.com/gpu -> 本地 CUDA Driver -> 本地 GPU
远端选中：Pod -> Lupine shim -> 网络 -> Lupine Server -> 远端 GPU
```

### 6\.2 不能自由组合的情况

- 一个业务容器不能同时加载 Lupine、Juice、Cricket 等多个远端 client；它们都会拦截 CUDA 调用。

- 一个业务容器不应同时申请 `nvidia.com/gpu` 又注入远端 CUDA shim，否则本地和远端设备枚举、库加载与显存归属会冲突。

- HAMi 软件切分、MIG 实例和远端整卡可以参与同一次启动前选择，但不能默认具有相同的隔离、性能和计量语义。

- Volcano/KAI 不能单独让远端 GPU 可被业务调用；它们必须与 Lupine/Juice/Cricket 等远端 runtime，以及一个选择器组合。

- DRA 只解决资源声明和绑定，不替代远端 CUDA runtime。

### 6\.3 组合 A：本地整卡 \+ Lupine \+ Webhook

```Plaintext
本地：GPU Operator + NVIDIA Device Plugin
远端：Lupine client/server
选择：Admission Webhook
```

|内容|说明|
|---|---|
|本地执行|Pod 直接申请 `nvidia.com/gpu`，走原生 NVIDIA runtime|
|远端执行|Pod 注入 Lupine shim 与远端 server 地址，CUDA/NVML 调用转到远端|
|切换方式|Webhook 在 Pod 创建前选择本地或远端配置|
|能表达的效果|本地优先或远端优先的启动前透明选择；CPU\-only 工作负载也可走远端|
|仓库复用|GPU Operator、Device Plugin、Lupine 均为开源；模式选择与 endpoint 预留需自行组合|

### 6\.4 组合 B：本地整卡/切分 \+ Lupine 或 Juice \+ Workload Controller

```Plaintext
本地：GPU Operator + Device Plugin + 可选 HAMi
远端：Lupine 或 Juice client/server
选择：逻辑 Workload + Controller + Remote Agent/租约
```

|内容|说明|
|---|---|
|本地执行|整卡可走 Device Plugin；切分或 MIG 可由 HAMi 参与调度和分配|
|远端执行|Controller 为每个任务选择具体 endpoint，再注入对应 remote runtime|
|切换方式|先绑定资源、保存绑定快照，再生成 Local Pod 或 Remote Pod|
|能表达的效果|启动前透明选择、可追踪的本地/远端绑定、多个远端 host 的资源归属|
|仓库复用|本地组件和远端 runtime 可直接组合；Agent、租约、会话、资源目录是组合层新增能力|

### 6\.5 组合 C：HAMi 本地共享 \+ Volcano/KAI 调度 \+ 远端 runtime

```Plaintext
本地：HAMi（GPU 共享、MIG、设备感知）
调度：Volcano 或 KAI Scheduler（队列、gang、公平、拓扑）
远端：Lupine / Juice / Cricket
选择：Controller 或 DRA/CRD
```

|内容|说明|
|---|---|
|本地执行|本地资源不只是一张整卡，也可以是 MIG 或受限的软件份额|
|远端执行|仍由独立远端 runtime 完成 CUDA API 透明转发|
|切换方式|切换前要比较的不只是“有无 GPU”，还包括整卡/MIG/共享份额是否符合请求的隔离和显存语义|
|能表达的效果|本地 GPU 利用率优化、队列化训练任务、资源碎片减少后再选择远端 GPU|
|仓库复用|HAMi、Volcano、KAI 均开源；它们与远端 runtime 的统一资源模型需要额外设计|

### 6\.6 组合 D：统一资源 Claim \+ 远端 runtime Provider

```Plaintext
资源描述：Kubernetes DRA 或自定义 CRD
本地：Device Plugin / HAMi
远端：Remote GPU endpoint provider + Lupine/Juice/Cricket
选择：资源驱动或 Controller
```

|内容|说明|
|---|---|
|本地执行|本地整卡、MIG 或切分实例以资源属性参与 claim 选择|
|远端执行|远端 endpoint 也以资源属性参与 claim，但最终仍需注入具体远端 runtime|
|切换方式|claim 绑定结果决定运行模式；资源 API 负责“选哪个”，runtime 负责“怎么调用”|
|能表达的效果|多资源池统一表达、配额与资源属性过滤、调度器可见的远端资源|
|仓库复用|DRA/KAI/HAMi 可用于资源声明与调度；远端 provider、session、runtime 装配需单独完成|

## 7\. 其他相关开源仓库：统一平台参考

### 仓库分析：TensorFusion

|项目|分析|
|---|---|
|仓库|[NexusGPU/tensor\-fusion](https://github.com/NexusGPU/tensor-fusion)|
|是否开源|主仓库为 Apache\-2\.0|
|公开方向|GPU 虚拟化、分数 GPU、远端 GPU sharing、调度、资源池、DRA、监控等都在项目公开路线中出现|
|在本文流程中的位置|覆盖本地共享、资源池、调度和远端调用等多个位置，属于“一体化平台形态”参考|
|可复用方式|对明确在仓库中发布的 chart、operator、provider、调度和资源模型进行代码级评估|
|需要区分的地方|README 将 GPU live migration、GPU context snapshot/restore、企业级 HA/安全等放在 Enterprise Features；这些描述不能仅凭主仓库开源许可证就认定为可直接复用的开源能力|
|调研价值|可用于研究“一个统一 GPU 平台应暴露哪些对象和组件”；具体模块仍需逐个检查源码、文档和许可证|

TensorFusion 与“把 GPU Operator \+ HAMi \+ 远端 runtime \+ 自定义选择器组合起来”的路线不同：前者试图形成统一产品形态，后者将不同职责的成熟项目组合。报告中可以保留它作为独立一体化路线，而不将其与单一远端 CUDA shim 等价比较。

## 8\. 开源复用总表

|仓库|是否开源|许可证|对应流程位置|可以直接复用的内容|不能替代的内容|
|---|---|---|---|---|---|
|NVIDIA GPU Operator|是|Apache\-2\.0|本地节点准备|驱动、runtime、Device Plugin、监控生命周期|远端调用、选址、切换|
|NVIDIA Device Plugin|是|Apache\-2\.0|本地设备分配|GPU 暴露、健康、Allocate|切分策略、远端 endpoint|
|HAMi|是|Apache\-2\.0|本地共享/切分/调度|分数卡、MIG、共享、设备感知调度|远端 CUDA RPC|
|Volcano|是|Apache\-2\.0|本地批调度|队列、gang、批任务调度|GPU\-over\-IP|
|KAI Scheduler|是|Apache\-2\.0|本地 AI 调度/DRA|队列、公平、GPU sharing、DRA、拓扑|CUDA API 远端转发|
|Kubernetes DRA|是|Kubernetes 原生 API|逻辑资源声明与绑定|ResourceClaim、DeviceClass、设备属性选择|远端 runtime 实现|
|Lupine|是|Apache\-2\.0|远端 runtime/client\-server|CUDA/NVML shim、远端调用、容器镜像|资源调度、endpoint 会话、租约|
|Juice Community Version|是|MIT|远端 runtime/client\-server|GPU\-over\-IP、跨环境部署形态|统一资源选择和平台审计|
|Cricket|是|MIT|远端 RPC runtime|RPC client/server 路线|通用安全、资源池、自动分配|
|SCUDA|是|Apache\-2\.0|远端 API codegen|CUDA RPC wrapper/codegen 参考|独立平台能力|
|mrCUDA|是|GPL\-3\.0|运行中迁移研究|迁移问题的研究参考|现代生产远端 GPU 链路|
|TensorFusion|是（主仓库）|Apache\-2\.0|一体化平台参考|公开的池化/调度/虚拟化模块|README 中企业功能不应自动视作可复用源码|

## 9\. 参考资料

- [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator)

- [NVIDIA k8s\-device\-plugin](https://github.com/NVIDIA/k8s-device-plugin)

- [HAMi](https://github.com/Project-HAMi/HAMi)

- [Volcano](https://github.com/volcano-sh/volcano)

- [KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler)

- [Kubernetes Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)

- [Lupine](https://github.com/lupinemachines/lupine)

- [Juice Community Version](https://github.com/Juice-Labs/Juice-Labs)

- [Cricket](https://github.com/RWTH-ACS/cricket)

- [SCUDA GPU\-over\-IP bridge](https://github.com/vn-os/scuda_GPUs-over-IP-bridge)

- [mrCUDA](https://github.com/EBD-CREST/mrCUDA)

- [TensorFusion](https://github.com/NexusGPU/tensor-fusion)

