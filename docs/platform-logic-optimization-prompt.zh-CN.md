# 异构加速平台控制面优化 Prompt

这是一份给 Codex、Claude 或其他代码代理使用的执行 Prompt。目标是优化现有平台的控制面逻辑，使本地 GPU、远端 CoreX 和远端 MLU 都具备可观察、可回收、可验证的资源生命周期。

## 可直接复制的 Prompt

你正在维护仓库 `/home/lvtong/vGPU/vGPU` 中的 `accelerator-control-plane`。当前平台已经完成以下数据面验证：

- CoreX 原生 Driver smoke 通过；
- CoreX Remote 客户端经 CRX9/TCP 连接服务端，kernel、D2H、数值校验和 session 清理通过；
- MLU Remote 的代表性镜像和调用链已通过；
- 控制面当前 `go test ./...` 通过；
- 控制面已经有 Provider、Pool、Endpoint、Workload、Lease、Local/Remote 选择、RuntimePlan、Fallback 和 Pod 注入模型；
- Provider 已包含 CoreX、MLU、Cricket 和 Local 等实现。

现在不要做性能优化，不要扩展 CUDA/MLU API，不要做运行中热迁移，不要重写整个控制器。重点完成以下四项：

1. 固化当前可运行基线；
2. 补齐真实平台 E2E 和故障场景；
3. 收敛控制器和 Provider 的重复逻辑；
4. 修复运行时镜像和运行库交付问题。

平台必须至少能够感知远端 endpoint 是否健康、是否被平台占用、是否存在活动 session，以及这些信息是否可信。不能把“没有探测到”当成“空闲”。

### 工程约束：保持简单、符合实际

这次优化以现有真实能力为边界，优先解决已经观察到的问题。不要为了“以后可能支持”提前设计复杂框架，也不要把控制面改造成通用调度器。

- 保持状态转换直接、可读、可测试；一个状态只保留一个事实来源。
- 复用现有 Provider、Allocator、RuntimePlan、Lease 和 Controller 接口；只有在现有接口无法表达已验证需求时才增加新接口。
- 不新增没有真实调用方的抽象层、插件层、策略层或配置项。
- 不重复校验 Kubernetes API 已经保证的结构，也不为理论上不可达的状态堆叠分支。
- 只在用户配置、跨组件边界、远端不可信数据和资源生命周期边界做必要校验。
- 不把所有错误都包装成可重试；重试必须有明确原因、上限和可观察结果。
- 不用默认值、缓存值或“没有报错”推断远端空闲。无法观测就明确标记 Unknown；这是事实语义，不是增加防御性分支。
- 不因为担心未来的并发、厂商或拓扑场景而提前实现未验证的兼容路径。
- 发现 CoreX、MLU 或 Cricket 的真实差异时，在 Provider 内做最小适配，不把厂商特例泄漏到通用 Controller。
- 每次新增校验、状态或重试都必须能对应一个现有故障、测试用例或明确的资源安全不变量；否则不要加入。

代码应优先表达平台现在能可靠证明的事实。对于尚未具备观测能力的内容，返回 Unknown 并记录原因，比伪造精确状态或引入一套复杂推断机制更符合实际。

### 一、先阅读并保护现有架构

先阅读：

- `accelerator-control-plane/api/v1alpha1/*_types.go`
- `accelerator-control-plane/internal/contracts/contracts.go`
- `accelerator-control-plane/internal/controller/workload_controller.go`
- `accelerator-control-plane/internal/controller/fallback.go`
- `accelerator-control-plane/internal/controller/configuration_controller.go`
- `accelerator-control-plane/internal/allocation/*`
- `accelerator-control-plane/internal/inventory/*`
- `accelerator-control-plane/internal/policy/local_first.go`
- `accelerator-control-plane/internal/provider/corex/provider.go`
- `accelerator-control-plane/internal/provider/mlu/provider.go`
- `accelerator-control-plane/internal/provider/cricket/provider.go`
- `accelerator-control-plane/internal/provider/local/provider.go`
- `accelerator-control-plane/internal/injector/pod.go`

先运行现有测试并记录基线：

```bash
cd /home/lvtong/vGPU/vGPU/accelerator-control-plane
go test ./...
```

不要覆盖或丢弃用户当前修改。开始前查看 `git status`，任何重构都必须保持可回退。

### 二、建立统一的远端占用和健康语义

为远端 Endpoint 建立明确的、Provider 无关的观察模型。可以新增内部 contracts 类型，也可以扩展已有 Endpoint status，但必须区分以下概念：

- `AdvertisedCapacity`：Endpoint 声明可以提供的设备数量；
- `Reserved`：控制面已经成功创建且未释放的 Lease 数量；
- `ActiveSessions`：远端服务实际报告的活动 session 数量；
- `Available`：在数据来源可信时计算出的可分配数量；
- `Health`：Healthy、Degraded、Unhealthy、Unknown；
- `ObservedAt`：观察时间；
- `Source`：static、lease-accounting、remote-probe、agent；
- `Confidence`：Observed、Derived、Unknown；
- `Reason` 和 `Message`：说明为什么可用、占用或不可判断。

不要把 `Reserved=0` 直接解释成远端设备空闲。对于没有真实 session/设备查询能力的 Provider，必须显示 `Unknown` 或 `Derived`，并继续使用 Lease 账本防止超卖。

建议 Endpoint status 至少能够表达：

```text
Configured
Ready
Health
Capacity
Reserved
ActiveSessions
Available
ObservedAt
ObservationSource
Reason
Message
```

如果需要新增字段，使用 Kubernetes status condition 习惯：设置 `ObservedGeneration`、`LastTransitionTime`、稳定的 `Reason`，避免每次 Reconcile 只更新时间造成无意义写入。

### 三、实现远端 Endpoint 的观察流程

把观察分成两层：

1. **控制面账本观察**：由 Lease 创建、绑定、释放和过期计算 `Reserved`。这一层必须强一致、可重试、幂等。
2. **远端运行时观察**：由 CoreX/MLU Remote Adapter 在具备能力时提供健康和活动 session 信息。无法提供时返回 `Unknown`，不能伪造空闲。

设计一个小而稳定的接口，例如：

```go
type EndpointObserver interface {
    Observe(ctx context.Context, endpoint contracts.RemoteEndpoint) (contracts.EndpointObservation, error)
}
```

接口返回值至少包括健康状态、容量、活动 session、观察时间、来源和可信度。不要让通用 Workload Controller 直接调用 CoreX、MLU、Cricket SDK 或 TCP 协议；厂商差异必须留在 Provider/Adapter 内。

观察失败时：

- 更新 Endpoint 为 `Unknown` 或 `Degraded`；
- 记录事件和可读原因；
- 默认禁止新的 Remote 分配，除非策略明确允许 stale inventory；
- 不自动删除已有 Lease；
- 不因为一次探测失败就释放真实资源；
- 继续通过 Lease 和 session 清理流程恢复。

### 四、让 Workload、Lease、Endpoint 状态互相可解释

实现以下不变量：

1. 一个活动 Remote Lease 对应一个明确的 Endpoint 和资源目标；
2. 同一 Lease 重复 Reconcile 不会增加第二次占用；
3. Lease 进入 Released、Failed 或 Expired 后，Reserved 最终减少一次；
4. Endpoint Unhealthy 时不能创建新的 Remote Pod；
5. Endpoint Unknown 时不能显示为 Available；
6. Pod 在 Ready 前失败时可以按既有 fallback 策略回退；
7. Pod 已经 Ready 后失败不能伪装成启动失败并自动迁移；
8. Controller 重启后可以从 Workload、Lease、Endpoint 和 Pod 恢复状态；
9. session 仍活跃但 Lease 已释放时，状态必须显示不一致并告警，不能静默覆盖；
10. Lease 仍存在但 session 已消失时，必须进入可回收或待确认状态，不能马上假设设备空闲。

Workload status 和 Events 至少要能回答：

- 选择了 Local 还是 Remote；
- 选择原因是什么；
- 当前占用了哪个 Pool、Endpoint 和 Lease；
- Endpoint 当前是否健康；
- 为什么排队、为什么拒绝、为什么回退；
- 资源什么时候释放以及释放是否成功。

### 五、收敛控制器复杂度

现有 `workload_controller.go` 较大。保持外部行为和 CRD 兼容，把内部逻辑按阶段拆开：

```text
selection
reservation
runtime-plan
pod-materialization
readiness-and-fallback
terminal-status
release-and-finalization
```

每个阶段都要满足：

- 输入和输出明确；
- 只负责一个状态转换；
- 重复调用安全；
- 错误可以分类为 Invalid、Unschedulable、Retryable、Permanent；
- 不直接混合 Pod 修改、Lease 修改和 status 修改。

不要把所有逻辑搬到新的“大服务”里。优先提取纯函数和小接口，保留现有 `SelectionRequest`、`Decision`、`ReservationRequest`、`RuntimePlan`、`Allocator` 和 `Provider` 的语义。

### 六、审查并适度瘦身适配层

当前代码同时存在 `ExecutionAdapter`、`adapter.Binding`、`adapter.Registry` 和 `contracts.ProviderRegistry`。它们分别有合理用途，但注册和查找存在重复视图；CoreX、MLU、Cricket 目前还共享同一个 RemoteAllocator。先画调用关系，再决定删减，不要凭感觉大面积重写。

允许做的瘦身方向：

- 保留一个注册表作为 Provider 和 Allocator 的权威来源；
- 用一个清晰的注册项表达 `{Provider, Allocator}` 绑定；
- Provider 视图可以从同一个注册表派生，不要维护两套可能不一致的 map；
- 将“同一个 driver 的 Provider 和 Allocator 必须成对解析”保留为注册表不变量；
- 删除只做转发、没有独立语义的 wrapper、重复 lookup 和重复 capability 校验；
- 保留小的 `Provider`、`Allocator` 接口，因为运行时物化和容量预留仍然是两个不同职责；
- 允许多个 Provider 共享一个 Allocator，但要在注册关系中明确表达，不能用“同一个后端”注释代替事实。

不要为了减少目录或文件数量而把 Provider、Allocator、Inventory、Policy 和 Controller 合成一个对象。不要引入继承式万能 Adapter，也不要为未来的动态插件加载提前保留复杂生命周期。

瘦身前后必须证明：Provider 配置校验、Allocator 选择、Endpoint 校验、Lease 释放和已有 Controller 测试的行为不变。只有确认某个接口没有独立调用方、语义或测试价值后，才删除它。

### 七、消除 CoreX/MLU Remote Provider 重复代码

CoreX 和 MLU Provider 中的下列逻辑应尽量共享：

- client runtime 版本校验；
- sourcePath/mountPath 校验；
- endpoint host/port 解析；
- init container 复制运行库；
- EmptyDir 和 VolumeMount 构造；
- LD_LIBRARY_PATH、LD_PRELOAD 和远端地址注入；
- RuntimePlan 的通用 annotation；
- 远端 Lease 必须是独占 Endpoint 的校验。

Provider 只保留真正的厂商差异：

- Driver 名称；
- 支持的 DeviceKind、ResourceForm 和 AllocationMode；
- 客户端库名称；
- 远端地址参数名；
- 厂商特有环境变量；
- 厂商特有的健康/活动 session 观察方式。

不要把 CoreX 和 MLU 的所有逻辑抽成一个无法阅读的万能 Provider。共享代码应有清晰的参数结构和单元测试。

### 八、修复运行时镜像交付

检查所有 CoreX/MLU workload、client runtime 和 server 镜像：

- workload 镜像必须明确包含应用、测试数据和所需客户端库，或者明确由 runtime init container 注入；
- 不能依赖开发机的隐式 hostPath 才能运行；
- `ldd`、`readelf -d`、`file` 必须能证明架构、SONAME 和 RPATH 正确；
- server 镜像必须包含其官方 CoreX/MLU 运行库，并明确设备访问方式；
- K3s/containerd 使用的镜像必须和 Docker 构建的镜像 digest 对齐；
- 所有 smoke 必须记录应用退出码和数值校验结果；
- 生产清单不能默认使用 `privileged: true`，除非实验配置明确标注并隔离。

### 九、必须增加的测试

至少增加以下单元测试、envtest 或真实 E2E：

1. 两个 Workload 并发竞争一个独占 Remote Endpoint，只有一个成功；
2. 第一个 Workload 结束后第二个自动获得资源；
3. Endpoint 健康变为 Unhealthy 后，新 Workload 保持 Pending；
4. Endpoint 探测超时后状态为 Unknown，不显示空闲；
5. Controller 重启后 Reserved 不重复累加；
6. 重复 Reconcile 不创建重复 Pod、Lease 或 session；
7. Remote runtime init container 失败时能够回退并释放原 Lease；
8. Pod 在 Ready 前失败时允许 fallback；
9. Pod 在 Ready 后失败时不触发启动 fallback；
10. Lease 过期、删除和 finalizer 清理不会泄漏远端占用；
11. 远端 session 与 Lease 不一致时产生明确告警；
12. CoreX 和 MLU 各执行一次真实镜像 RemoteOnly smoke，包含 H2D、kernel、同步、D2H、CPU 数值校验、退出码和 session cleanup。

每个真实 E2E 都要保存：

```text
workload status
lease status
endpoint status
pod describe/events
client stdout/stderr
server stdout/stderr
numeric result
exit code
cleanup result
```

### 十、明确非目标

本轮不要做：

- CUDA/MLU API 扩展；
- kernel 或 RPC 性能优化；
- 运行中热迁移；
- 多 Endpoint 聚合成一个任务；
- 任意跨设备 P2P；
- 没有实验依据的自动健康判断；
- 删除现有 CRD；
- 为了减少文件数量而合并 Provider、Allocator 和 Controller；
- 把一次 smoke 通过写成“生产可用”。

### 十一、完成标准

完成后必须输出：

1. 修改文件列表和每个修改的目的；
2. Endpoint 占用/健康语义说明；
3. CoreX、MLU、Local 三条路径的状态机说明；
4. 新增或修改的 CRD status 字段；
5. 并发、重启、故障回退和回收测试结果；
6. 镜像构建、架构、动态库和运行方式验证结果；
7. 明确仍未解决的问题；
8. `go test ./...`、生成代码检查、CRD 校验和 E2E 结果。

最终验收不是“程序打印 Ready”，而是：

```text
Endpoint 状态可信
Lease 不超卖、不重复、不泄漏
Workload 选择和回退有原因可查
Pod 运行时注入可复现
真实 kernel 数值正确
session 和 Lease 最终一致或显式告警
```

如果发现当前远端运行时没有提供真实的活动 session 或设备占用接口，要在报告中明确写出：当前只能提供 Lease 派生占用，远端真实占用为 Unknown；然后优先实现最小的 server health/session observation 接口，而不是在控制器里猜测。

## 执行顺序

按以下顺序工作，不要跳步：

1. 阅读代码、运行现有测试、保存基线；
2. 画出现有 Workload/Lease/Endpoint 状态转换和实际写入位置；
3. 先实现观察模型和状态语义；
4. 添加占用、健康、重启和回收测试；
5. 抽取 CoreX/MLU Remote Provider 共享构造逻辑；
6. 画出 Adapter/Provider/Allocator 的调用关系，合并重复注册表并删除没有独立语义的包装；
7. 拆分大 Controller 的内部阶段；
8. 修复并验证镜像交付；
9. 运行完整测试并更新文档；
10. 最后再由人工审查是否需要进一步重构。

