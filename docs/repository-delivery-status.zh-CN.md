# 当前仓库能力与交付管理总览

> 盘点日期：2026-07-31。
>
> 盘点依据：当前工作树中的代码、CRD、样例、自动化脚本和已归档的实机实验记录。
> 本文用于管理交付范围；它不替代版本发布说明、测试报告或生产运行手册。

## 1. 一句话结论

当前仓库已经不是只有设计稿的原型，而是已经具备以下三层能力：

```text
独立 Cricket 数据面
    +
本地/远端统一的平台适配控制面
    +
Cricket 作为 Remote 执行适配器的真实接入与端到端样例
```

其中，Cricket 已经接入平台适配层，不是预留接口或 mock：`cmd/manager/main.go` 在启动时注册本地 Device Plugin Adapter 和 Cricket Adapter；控制器会选择其中一个 Adapter，建立 Lease、生成 RuntimePlan，并把结果物化为业务 Pod。

当前最重要的管理结论是：能力代码、部署清单、样例和文档均在本地工作树中，但尚未固化为一个干净、可追溯的 Git 交付版本。交付前首先应完成测试、生成物核对和提交切分。

## 2. 状态标签与边界

本文使用四种状态：

| 状态 | 含义 |
|---|---|
| 已实现 | 代码、接口与配置已存在，并有相应测试或静态校验入口 |
| 已实测 | 除代码外，已有真实命令输出或端到端实验记录 |
| 可复现 | 样例、构建/部署脚本已提供；应在目标环境重新运行 |
| 待生产化 | PoC/真实 E2E 已经证明方向，但仍缺少稳定运行所需能力 |

本仓库负责选择、预留、注入、运行与回收的控制面语义。
它不负责替代 NVIDIA Device Plugin、创建或切分 MIG、实现 HAMi 资源隔离，也不负责 Cricket server 的自动拉起。

## 3. 总体实现地图

```text
AcceleratorWorkload
  -> InventoryReader
       -> Local：Node allocatable - Pod request - active Lease
       -> Remote：Endpoint devices/health - active Lease
  -> LocalFirst Policy
  -> AcceleratorLease（不可变决策与绑定快照）
  -> ExecutionAdapter（Provider + Allocator）
       -> Local：Kubernetes Device Plugin 扩展资源
       -> Cricket：Endpoint claim + RuntimePlan
  -> PodMutator
       -> Local：resources + Node 约束
       -> Cricket：init runtime + loader 环境变量
  -> Pod 完成 / Lease Released
```

代码布局与职责：

| 目录/文件 | 已完成内容 |
|---|---|
| `api/v1alpha1` | 五类 CRD API：Provider、Pool、Endpoint、Workload、Lease；状态、回退与容量模型 |
| `internal/contracts` | Provider、Allocator、ExecutionAdapter、RuntimePlan、库存与策略的稳定边界 |
| `internal/adapter` | Adapter Registry；确保一个 driver 的 Provider 与 Allocator 一起注册 |
| `internal/controller` | Workload 生命周期、配置校验、事件、启动前回退、确定性 Pod 命名 |
| `internal/inventory` | 本地 Node/Pod/Lease 与远端 Endpoint/Lease 的可用容量汇总 |
| `internal/policy` | LocalFirst、LocalOnly、RemoteOnly、Auto 顺序与失败目标排除 |
| `internal/allocation` | 本地预留、远端 Endpoint 解析、原子 claim、释放与状态机 |
| `internal/provider/local` | 本地扩展资源 RuntimePlan |
| `internal/provider/cricket` | Cricket Remote RuntimePlan、版本和端点参数校验 |
| `internal/injector` | RuntimePlan 到 PodTemplate 的幂等注入与冲突检测 |
| `config` | CRD、RBAC、Deployment、Kustomize、样例配置 |
| `examples/cricket-e2e` | 基础远端 kernel、图像任务、镜像和完整演示脚本 |

## 4. 平台适配层：已完成内容

### 4.1 统一执行适配边界

`ExecutionAdapter` 由两部分组成：

```text
Provider  -> 校验配置并根据已绑定 Lease 生成 RuntimePlan
Allocator -> Reserve / Bind / Release 对应资源容量
```

Adapter Registry 会拒绝缺少任一半边、driver 重复或能力声明不完整的实现。
因此控制器不直接依赖 CUDA、RPC 或厂商 SDK；新运行时只需实现 Provider 与 Allocator 即可接入。

当前内置 Adapter：

| driver | 模式 | Provider | Allocator | 当前状态 |
|---|---|---|---|---|
| `kubernetes-device-plugin` | Local | 本地扩展资源计划 | 本地 Lease 预留 | 已实现；真实 Device Plugin 待硬件验收 |
| `cricket` | Remote | Cricket runtime 注入计划 | Endpoint 原子独占 | 已实现并有真实远端 CUDA 记录 |

入口见：[manager 注册逻辑](../cmd/manager/main.go)、[Adapter Registry](../internal/adapter/registry.go)、[通用契约](../internal/contracts/contracts.go)。

### 4.2 API 与资源模型

| 对象 | 范围 | 已完成职责 |
|---|---|---|
| `AcceleratorProvider` | 集群 | 声明 driver、设备种类和客户端运行时信息 |
| `AcceleratorPool` | 集群 | 描述 Local 或 Remote 资源池、优先级和资源形态 |
| `AcceleratorEndpoint` | 集群 | 声明静态远端地址、runtime、设备和健康信息 |
| `AcceleratorWorkload` | 命名空间 | 用户请求、放置策略、回退策略和 PodTemplate |
| `AcceleratorLease` | 命名空间 | 不可变选择、预留、绑定、释放和失败历史 |

CRD 已生成 OpenAPI 和 DeepCopy；外部 `PodTemplateSpec` 的 labels/annotations schema 通过 `config/crd/patches/acceleratorworkload-podtemplate-metadata.yaml` 补齐，并已有 server-side dry-run 验证入口。

### 4.3 本地资源路径

Local Provider 使用 Kubernetes Device Plugin 发布的扩展资源，不直接发明新的设备分配协议。
当前支持的控制面语义是：

- `WholeDevice`、`MIG`、`Shared`、`Custom` 四类资源形态；
- Node selector、厂商、型号、profile、隔离级别和属性过滤；
- `resource.Quantity` 精确容量计算；
- 计入现有 Pod、init container、Pod overhead 和活动 Lease；
- RuntimePlan 注入资源 requests/limits 与已选择 Node 的约束；
- 相同资源名跨 Pool 的预留记账，避免逻辑超卖。

这意味着控制面已经能表达、选择和记账多形态本地资源。
它不意味着当前环境已经验证了真实 NVIDIA MIG、HAMi 或其他共享插件的物理隔离语义。

### 4.4 调度、Lease 与一致性

已实现：

- `LocalOnly`、`RemoteOnly`、`Auto` 及 LocalFirst 顺序；
- Pool 优先级、候选过滤和失败目标排除；
- 不可变 Lease target、资源快照和 Endpoint binding snapshot；
- 确定性 Pod 名称和幂等 reconcile；
- Pod 创建结果未知时保留预留，避免可能仍在运行的 Pod 被提前释放；
- Workload/Lease 删除、终态和过期时的回收路径。

远端 Endpoint 最终占用不依赖缓存库存：`KubernetesEndpointClaimer` 使用
`coordination.k8s.io/Lease` 的 API Server Create 作为 CAS。holder 使用
`AcceleratorLease.UID`；不同 holder 并发申请同一 Endpoint 时只有一个成功。
这解决缓存陈旧、重复 reconcile 和 leader 切换造成的双重分配风险。

### 4.5 启动前回退

S2 回退已经实现，但边界刻意收紧：

- 只在首次 Ready 前执行；
- 覆盖不可调度、Endpoint Ready 失效和平台注入的 runtime init 失败；
- 使用持久化失败历史、次数预算和绝对 backoff；
- 可 Local -> Remote，也可 Remote -> Local；
- 应用进程自身退出、用户 init container 失败、Ready 后失败不自动重放。

这样避免把有副作用的普通业务错误误认为资源故障。

### 4.6 RuntimePlan 与 Pod 注入

`RuntimePlan` 可以表达 init container、volume、环境变量、资源请求、Node 约束和审计注解。
`PodMutator` 幂等合并这些字段，并拒绝破坏性冲突，例如用户预先声明不同的受管
`LD_PRELOAD`、`LD_LIBRARY_PATH`、volume 或资源请求。

平台适配层的详细实现说明见：[platform-adapter-implementation.zh-CN.md](platform-adapter-implementation.zh-CN.md)。

## 5. 独立 Cricket 数据面：已完成内容

这部分不依赖控制面。它证明的是“CUDA 程序可以经 Cricket 调用远端 GPU”。

### 5.1 已完成部署链路

```text
动态链接 CUDA 应用
  -> LD_PRELOAD=cricket-client.so
  -> ONC RPC/TCP
  -> rpcbind / cricket-rpc-server
  -> 远端 CUDA Runtime、Driver 与 GPU
```

已固定：

- Cricket commit `5d55bd582fc7acbb2a12c45f104020566a67218a`；
- libtirpc commit `5ca4ca92f629d9d83e83544b9239abaaacf0a527`；
- CUDA 12.8 / cuDNN 9.12 的验证组合；
- RTX 5060 Ti、WSL GPU、`rpcbind` 和 `cricket-rpc-server` 的真实环境。

已完成的手工数据面验证：

| 实验 | 覆盖内容 | 已记录结果 |
|---|---|---|
| 运行库检查 | `nvidia-smi`、nvcc、CUDA/cuDNN 库 | GPU 与开发/运行库可用 |
| API smoke | device count、malloc、memset、free | 返回码均为 0 |
| kernel smoke | kernel launch、同步 | 返回码为 0 |
| 正确性 | H2D、kernel、D2H、CPU 校验 | `verification failures: 0`、样本 `1 2 256` |
| LAN 入口 | TCP 111、动态 Cricket 端口 | 调用端可达 |

独立调用、构建与测试报告见：[cricket-standalone-remote-call.zh-CN.md](cricket-standalone-remote-call.zh-CN.md)。

### 5.2 网络与远端主机

已形成两类部署方法：

- Linux 到 Linux：远端直接用 LAN IP 和防火墙规则开放 TCP 111/实际 RPC 端口；
- Windows + WSL2：Windows LAN IP 通过 `netsh interface portproxy` 转发到 WSL IP，Windows 防火墙限制调用端来源。

WSL 地址与 Cricket 动态端口可能在重启后改变，因此端口转发和 Endpoint 地址不能被视为永久静态配置。
网络、路由器、DHCP/静态 IP、无线隔离和端口转发的操作手册见：[network-two-lan-configuration.zh-CN.md](network-two-lan-configuration.zh-CN.md)。

## 6. Cricket 已接入平台适配层

这是独立 Cricket 数据面之上的第二层，当前已经完成真实接入。

### 6.1 接入路径

```text
AcceleratorWorkload（RemoteOnly / Auto）
  -> Policy 选中 cricket Remote Pool
  -> 创建 AcceleratorLease
  -> RemoteAllocator 原子 claim Endpoint
  -> Cricket Provider 构造 RuntimePlan
  -> init container 复制 client runtime 到 emptyDir
  -> 注入地址、RPC version、LD_PRELOAD、LD_LIBRARY_PATH
  -> 业务进程发起普通 CUDA 调用
  -> cricket-client.so 转发到远端 GPU
  -> Pod 完成，Lease 与 Endpoint claim 释放
```

Cricket Provider 注入的关键内容：

```text
REMOTE_GPU_ADDRESS=<已绑定 Endpoint 地址>
CRICKET_RPCID=<Endpoint rpcID>
LD_PRELOAD=/opt/accelerator/cricket/cricket-client.so
LD_LIBRARY_PATH=/opt/accelerator/cricket
```

Provider 会校验：

- Endpoint 地址、driver、runtime version、API version 与 `rpcID`；
- client runtime version 与 Endpoint runtime version 精确一致；
- GPU、WholeDevice、ExclusiveEndpoint 限制；
- IPv4 地址格式；
- TLS/Auth 配置不被上游 Cricket 支持时明确拒绝；
- 用户 Loader 环境变量和平台计划不冲突。

实现入口：[Cricket Provider](../internal/provider/cricket/provider.go)、[Remote Allocator](../internal/allocation/remote.go)、[Endpoint Claim](../internal/allocation/kubernetes_claims.go)、[Pod Mutator](../internal/injector/pod.go)。

### 6.2 远端资源约束

当前 Cricket Adapter 的精确能力是：

| 项目 | 当前行为 |
|---|---|
| 设备类型 | GPU |
| 资源形态 | WholeDevice |
| 分配粒度 | 一个静态 Endpoint，独占 |
| 多 GPU 主机 | 每个独立可分配组启动受限 server，并声明独立 Endpoint |
| 设备隐藏 | 请求数必须等于该静态 server 暴露的健康设备数 |
| 通信 | IPv4 TCP/ONC RPC |
| 加密与认证 | 上游 Cricket 不支持，Provider 拒绝错误配置 |

因此它不能在一个多卡静态 server 中把其中一张卡隐蔽地租给某个 Workload；也不支持 MIG、fractional GPU、会话级共享或动态 server session。

### 6.3 已有端到端样例

基础样例：

- [remote-cricket-5060ti.yaml](../config/samples/remote-cricket-5060ti.yaml)：Provider、Pool、Endpoint、Workload；
- [run-demo.sh](../examples/cricket-e2e/run-demo.sh)：等待 `Succeeded`，输出 Workload、Lease、Pod 标签和远端 CUDA 日志；
- [README.zh-CN.md](../examples/cricket-e2e/README.zh-CN.md)：kind/WSL 或 PowerShell 构建与部署步骤。

复杂样例提供了三种实质 CUDA 任务：

| 任务 | GPU 流程 | 结果校验 |
|---|---|---|
| 图像管线 | RGBA、灰度、Gaussian、Sobel、直方图 | 边缘图与 histogram checksum |
| 批量缩放 | 多张图双线性缩放、RGBA 统计 | CPU reference 与均值 |
| 图像分割 | Gaussian、阈值、形态学处理 | mask、前景像素与 CPU reference |

这些样例通过普通 `AcceleratorWorkload` 运行，而不是让业务镜像自己设置远端地址或 loader 变量。
2026-07-31 已在刚构建、导入 kind 并滚动更新的 controller 镜像上重新执行：三个 `RemoteOnly` 图像任务和 `Auto` 任务均得到 `selectedMode: Remote`、`verification failures: 0` 与 `Lease: Released`。其中批量缩放、图像管线、图像分割分别验证了多 kernel、6 MiB 级拷贝、CPU reference、checksum 与前景像素结果。

另外新增了 [合成 LocalOnly 容器样例](../examples/local-synthetic-e2e/README.zh-CN.md)：它在测试 kind Node 上临时发布 `example.com/synthetic-gpu`，验证本地 Adapter 的 Local 选择、Node 绑定、requests/limits 注入和“不注入 Cricket”。这证明本地控制路径的容器契约，但不替代真实 Device Plugin/GPU/MIG 的硬件验收。

复现入口：[复杂样例说明](../examples/cricket-e2e/complex-demo/README.zh-CN.md)、[平台测试矩阵脚本](../examples/cricket-e2e/complex-demo/run-platform-matrix.sh)。

## 7. 测试与证据管理

### 7.1 测试资产

当前树中有 17 个 Go 测试文件、64 个 `Test*` 函数，覆盖：

- Adapter Registry 与能力声明；
- Local/Cricket Provider 的 RuntimePlan 与非法配置；
- LocalAllocator、RemoteAllocator、Endpoint CAS、孤儿 claim 回收；
- Inventory 容量、跨 Pool 记账与 Quantity 精度；
- LocalFirst 策略、候选排除与资源形态；
- Pod 注入幂等和环境/volume/resource 冲突；
- Workload 生命周期、S2 回退、重建、删除回收与 Ready 边界；
- controller-runtime envtest 的 CRD、默认值和原子 claim。

测试入口：

```bash
make test
make envtest
make verify
```

2026-07-31 已在本机 WSL `Ubuntu` 中恢复并核验 Go 1.26.5、Docker Engine 29.1.3、kind 0.32.0 与 kubectl 1.36.2。kind 的 `cricket-e2e-control-plane` 节点为 `Ready`，`/readyz` 返回 `ok`。

本轮已通过 `make verify`，即 `go fmt ./...`、`go vet ./...`、全量 `go test ./...` 和 manager 构建均通过。`make generate` 与 `make manifests` 连续执行两次后，DeepCopy 及五份 CRD bases 的 SHA-256 完全一致；`git diff --check` 通过。

本轮还通过了 `make envtest`：Kubernetes 1.36.2 的 API server/etcd 资产经 Windows 网关的 7890 代理下载后，`TestCRDsAndAtomicEndpointClaimsWithEnvtest` 成功运行。首次直连下载约 185 秒后超时，问题属于下载路径/代理，不是测试失败。

Docker 层也已重新验证：`make container-build` 生成 `accelerator-control-plane:dev`（本轮镜像 ID `sha256:ae222a4e…`），导入 `cricket-e2e` 并滚动更新两个 manager 副本后，`deploy.sh`、复杂 RemoteOnly/Auto 平台矩阵与合成 LocalOnly 容器契约均通过。`deploy.sh` 与 `deploy.ps1` 现会在重复演示时仅重置其自身的 `remote-kernel-test` 样例，因为已经分配的 `AcceleratorWorkload.spec` 按设计不可变。

重建 Docker daemon/镜像期间，kind API server 曾短暂超时，导致 leader-election manager 按预期退出并由 Deployment 重启；日志中没有 panic。随后两个副本在连续 45 秒观察中均保持 `1/1 Running`，重启计数不再增长且 `/readyz` 持续 `ok`。这可作为开发机环境抖动的已知记录，不能替代 P2 中的长时间 leader 切换与 API 超时验收。

WSL 工具链、Docker daemon 恢复方式、代理与非破坏校验命令见 [wsl-development-environment.zh-CN.md](wsl-development-environment.zh-CN.md)。

### 7.2 验收层次

| 层次 | 证明什么 | 主要入口 |
|---|---|---|
| 单元测试 | 控制面状态机、选择、分配、注入和错误分类 | `go test ./...` |
| envtest | 真实 Kubernetes API/CRD/Lease CAS 语义 | `make envtest` |
| 独立 Cricket | server、RPC、网络、CUDA 数据面 | standalone 文档的 smoke 命令 |
| 基础平台 E2E | 选择、注入、执行、Lease 释放 | `examples/cricket-e2e/run-demo.sh` |
| 复杂平台 E2E | 多 kernel、数据拷贝、CPU/GPU reference | `complex-demo/run-platform-matrix.sh` |
| 合成本地容器 E2E | Local 选择、Node 绑定、扩展资源注入、无远端 runtime | `examples/local-synthetic-e2e/run.sh` |
| 真实本地资源 | Device Plugin/MIG/共享资源的硬件隔离 | 待目标集群验收 |

交付时，报告必须分别保留这五层证据。不能用“独立 CUDA 成功”替代控制面测试，也不能用“Pod 成功启动”替代数值正确性测试。

## 8. 文档与操作入口

| 读者目标 | 入口 |
|---|---|
| 了解仓库当前完成度和优先级 | 本文 |
| 配置 WSL 中的 Go、Docker、kind、kubectl 与 envtest | [wsl-development-environment.zh-CN.md](wsl-development-environment.zh-CN.md) |
| 理解平台适配层的实现 | [platform-adapter-implementation.zh-CN.md](platform-adapter-implementation.zh-CN.md) |
| 独立部署 Cricket 并直接调用远端 GPU | [cricket-standalone-remote-call.zh-CN.md](cricket-standalone-remote-call.zh-CN.md) |
| 配置两台主机、路由器、WSL 转发与防火墙 | [network-two-lan-configuration.zh-CN.md](network-two-lan-configuration.zh-CN.md) |
| 理解 Cricket 接入控制面的运行时契约 | [cricket-integration.md](cricket-integration.md) |
| 复现当前调用与验收链路 | [cricket-runtime-call-and-validation-report.zh-CN.md](cricket-runtime-call-and-validation-report.zh-CN.md) |
| 使用平台提交远端任务 | [cricket-seamless-switching-user-guide.zh-CN.md](cricket-seamless-switching-user-guide.zh-CN.md) |
| 运行复杂图像演示与矩阵 | [complex-demo/README.zh-CN.md](../examples/cricket-e2e/complex-demo/README.zh-CN.md) |
| 无本地 GPU 时验证 Local Adapter 容器契约 | [local-synthetic-e2e/README.zh-CN.md](../examples/local-synthetic-e2e/README.zh-CN.md) |
| 了解阶段、风险和后续路线 | [implementation-roadmap.zh-CN.md](implementation-roadmap.zh-CN.md) |

建议以后按该表维护文档：每新增一个执行 Adapter，至少补齐“独立数据面说明、接入契约、样例、测试矩阵、限制清单”五项。

## 9. 当前交付风险与管理优先级

### P0：先固化当前工作树

本次整理完成时，工作树有 81 项变化：41 个已跟踪文件被修改，40 个文件或目录尚未跟踪（其中新增的 WSL 环境说明也是未跟踪文档之一）。
当前分支最近提交只包含接口基线与早期 Cricket Provider；当前完整能力不能仅凭已有 Git commit 复现。

在不覆盖其他本地修改的前提下，建议按以下顺序处理：

1. 已完成 WSL Go/Docker/kind 基线、`make verify`、`make envtest`、生成物幂等检查、CRD dry-run、当前 controller 镜像滚动更新、RemoteOnly/Auto 容器矩阵和合成 LocalOnly 容器契约；
2. 固定此次验证所用的 controller、Cricket client runtime 与业务镜像到不可变 tag/digest，并将其写入发布记录；
3. 按“核心控制面 / 部署与样例 / 文档与报告”审阅并拆分当前混合工作树的修改；
4. 给经测试的组合打 Git tag，并记录远端 Endpoint、CUDA/Cricket 版本和验收日期；
5. 不要直接对当前脏工作树执行无差别 `git add -A` 或 reset，应先确认哪些修改属于本次交付。

### P1：让远端服务可持续运行

生产前必须补齐：

- Cricket server 与 rpcbind 的 WSL/Linux 服务托管、重启和日志轮转；
- WSL IP 与动态 RPC 端口变化后的 portproxy 自动修复；
- Endpoint 网络/RPC 探针、`Ready`、`lastProbeTime` 和失效时效；
- 私有网络、防火墙、VLAN/VPN 与访问控制；
- 镜像仓库、不可变 tag/digest、拉取凭据和版本矩阵；当前开发机仍使用 Docker legacy builder，CI/交付前应安装 Buildx 并迁移 BuildKit。

### P2：真实硬件与运维验收

- 本地 NVIDIA Device Plugin、MIG、HAMi/共享插件的真实资源与隔离；
- 多副本 leader 切换、API 超时、claim 孤儿回收和 Pod 删除阻塞；
- CUDA、驱动、Cricket、cuDNN、框架和实际算子的兼容矩阵；
- Prometheus 指标、容量趋势、告警、SLO、quota 与审计；
- 升级、回滚、备份与事故处理手册。

### P3：明确延后范围

以下不应作为当前交付承诺：运行中迁移/检查点恢复、远端 MIG/fractional GPU、会话级共享、多厂商 NPU 数据面、跨不可信网络的原生加密认证。

## 10. 可作为当前交付的表述

推荐对外表述如下：

> 已完成一个可扩展的 Kubernetes 加速器控制面，统一管理本地 Device Plugin 资源与远端静态 Cricket GPU Endpoint。系统通过 Provider + Allocator Adapter 边界完成选择、Lease 预留、运行时注入和回收；Cricket 已在真实 Windows/WSL RTX 5060 Ti 环境完成 CUDA kernel、数据回传、数值校验和平台端到端验证。当前交付定位为可复现的研发/PoC 基线，生产化仍需补远端服务生命周期、健康探针、安全、镜像供应链、真实本地多形态资源与兼容性矩阵。

这段表述既准确反映已完成能力，也不会把仍待硬件或生产环境验证的部分提前承诺出去。
