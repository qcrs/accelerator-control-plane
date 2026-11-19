# 平台正确性审查与最小修复（2026-11-19）

## 范围与结论

以提交 `8800bba` 为本轮基线。先增加针对性用例，记录失败，再修改已有生命周期与注入函数。原有 [13/13 K3s 验收](k3s-validation-report.zh-CN.md) 保留为真实执行基线；本轮新控制器已另行完成 [13/13 矩阵](evidence/correctness-review-20261119/k3s-final/summary.json) 与 [独立复核](evidence/correctness-review-20261119/k3s-final/audit.json)，均通过。

没有修改对外 API、CRD、Remote Runtime 协议、Provider/Allocator 接口、Registry、Claim 原子操作或状态乐观锁。没有扩展 CUDA/MLU API。

| 清单项 | 核实结果 | 本轮处理 |
| --- | --- | --- |
| P0：成功但未观察到 Ready | 短任务正常完成且及时观察时原代码已能成功；超过启动期限才观察到 Succeeded，或此时后端不可用，才会误判启动失败 | Pod 成功终态优先；已失败的应用也不因超时或当前后端健康而重放。托管运行库 init 失败仍允许既有 fallback |
| P1：Claim 失败方清理 | 使用真实 KubernetesEndpointClaimer 实现和 fake API 存储复现：竞争失败后进入 fallback，下一轮非持有者 Release Conflict 卡住 | 复用已持久化的预留拒绝结果，只跳过确认未取得 Claim 的清理；不忽略通用 Release Conflict |
| P1：本地亲和性 | 合法 fixture 复现空 term 被当作匹配全部、Node name 被当成 hostname 标签，以及空 OR 分支被注入放宽 | Inventory 的空 term 不匹配节点；Injector 以 metadata.name 限定目标，保留用户标签与非空 OR 条件，拒绝全空 required affinity |
| P1：物理设备重复 | 只读现网检查：一个 CoreX Endpoint、一个 MLU Endpoint，各一个后端 Pod；设备路径不重叠 | 保留已有 UUID 重复校验，没有增加身份字段或新约束 |
| P2：Controller 集中 | 原文件 1,388 行，已有明确生命周期阶段 | 移动到选择、预留、Pod 构造、状态同步、释放五个文件；入口文件降至 444 行 |
| P2：Provider 重复 | 两者的运行库复制、EmptyDir、只读挂载和路径校验一致 | 提取到 internal/provider/runtime.go；环境变量、Shim、地址解析和协议适配仍归各 Provider |
| P2：查询与并发 | 当前指标快照没有显示工作队列积压；没有完成吞吐压测 | 保持全量查询和 MaxConcurrentReconciles=1；不据小样本增加索引或并发 |
| P2：应用环境冲突 | 现有 Injector 已有确定的兼容规则，未发现本轮真实应用冲突 | 记录规则，保持行为 |
| P3：排查信息 | 已有 Decision、Conditions、Events 和控制器错误日志 | 记录查询路径，没有增加第二套状态模型 |

## P0：完成结果先于当前启动条件

原 `syncBoundPod` 在处理 Pod 终态前，先检查远端健康和启动超时。这两项条件可能在计算成功之后才被观察到，不能推翻 Pod 已记录的完成结果。

现在终态 Pod 不进入当前远端健康检查；`detectStartupFailure` 不把 Succeeded 或应用已失败后的超时判作启动失败。Failed Pod 仍会先检查托管运行库 init 的明确失败原因，保留启动回退。未观察到 Ready 不会被伪造为 Ready，原 Pod 和结果保留，正常释放 Lease。

针对性回归覆盖：快速成功对照、超时后首次观察成功、远端故障后观察成功、应用失败叠加超时和故障、Failed 托管 init 仍能回退。原代码在前述条件下的失败见 [有效 fixture 的修复前记录](evidence/correctness-review-20261119/regressions-before-valid-fixture.log)。

## P1：三种所有权事实

| 可确认的事实 | 现有状态依据 | 清理行为 |
| --- | --- | --- |
| 未取得 Claim，或已经确认回滚 | Remote Pending Lease 无 binding，Reserved=False/CapacityChanged；RemoteAllocator 的非 Transient 拒绝不保留该请求的所有权 | 结束这个未取得资源的分配，不对赢家调用 Release |
| 取得或回滚结果不确定 | ReservationRetrying；包括 API 传输失败或回滚失败 | 保留 Lease 与 finalizer，仍需调用 Allocator 确认清理；不能宣告 Released |
| 已取得资源 | 持久化的 binding 或已有活动分配 | 沿原 Release 路径核对持有者并使用 UID/resourceVersion 删除前置条件 |

拒绝结果必须在重启后仍可信。如果 Lease 的 CapacityChanged 已保存、Workload 的失败历史写入发生 Conflict，恢复时应补记这个拒绝结果，不能用同一个 Lease 再次调用 Reserve。新增用例通过真实 resourceVersion 冲突复现了重复预留，修复后同时验证没有新 Claim、fallback 只消耗一次。见 [拒绝结果恢复的修复前记录](evidence/correctness-review-20261119/rejection-status-before.log)。

回归还分别验证：竞争赢家的 Claim 完全不被修改；控制器重建后失败方能切到 Local；不确定和已取得所有权两种情况下的非持有者 Conflict 都仍保留 finalizer。`KubernetesEndpointClaimer.Release` 本身没有放宽。

## P1：调度语义与身份核查

Kubernetes 定义空 NodeSelectorTerm 不匹配任何节点，hostname 标签也不保证等于 Node name。[NodeSelectorTerm 定义](https://kubernetes.io/docs/reference/kubernetes-api/definitions/node-selector-term-v1/)；[节点标签说明](https://kubernetes.io/docs/reference/node/node-labels/)。

Inventory 过滤和 Injector 物化现在通过同一组跨组件用例验证：被选中的合法节点在注入后仍能匹配；另一个同标签节点不能接收该分配；原 zone 条件不会因为空 OR 分支而放宽；不使用 spec.nodeName 绕过调度器。

现网核查见 [拓扑快照](evidence/correctness-review-20261119/endpoint-topology.json)。CoreX 挂载 `/dev/iluvatar0`；MLU 挂载 `/dev/cambricon_*`。现有 `DeviceInfo.UUID`、配置控制器跨 Endpoint 重复校验和 Inventory 重复 UUID 拒绝逻辑均已存在，本轮不重复实现。设备 ID 仍是 runtime 内部 ID，未填 UUID 的配置不能由此证明全局物理身份；该快照也不覆盖集群外的进程。

## P2：仅收敛重复实现

- `workload_selection.go`：选择和恢复原有 Lease 决策。
- `workload_reservation.go`：预留、拒绝结果恢复和配置丢失后的清理。
- `workload_pod.go`：RuntimePlan 物化、Pod 创建和构造辅助函数。
- `workload_status.go`：Pod 状态、Ready/终态与远端状态同步。
- `workload_release.go`：过期、删除、资源释放和 finalizer。

分文件时对 24 个被移动函数及其余原函数的函数体逐一做 SHA256 对照，没有重写生命周期。之后的 `reserveLease` 拒绝结果恢复是有独立失败用例的功能修复，另行标注在 [拆分记录](evidence/correctness-review-20261119/controller-split.json)，不把它混作纯移动。

Provider 共享函数只返回现有 RuntimePlan 的复制和挂载部分，并校验原有路径规则；厂商默认路径与错误上下文仍由 Provider 提供。CoreX 的直接 DNS、MLU 的两个 Shim、LD_PRELOAD、LD_LIBRARY_PATH 和 runtimeVersion 规则保持原样。

现网旧版本单个 manager Pod 的 [只读指标快照](evidence/correctness-review-20261119/manager-metrics-before.json) 中，Workload 的 63 次 Reconcile 平均约 19.48 ms，Pool 的 3,148 次平均约 0.32 ms，采样时工作队列深度均为零。这些是累计 Reconcile 指标，不是单条 Inventory 查询耗时，不是吞吐压测，也不能推断提高并发的收益。本轮保留原查询方式和单线程设置。

## 环境兼容与排查

Injector 对同名、完全相同的 EnvVar 接受重复注入；同名但 Value 或 ValueFrom 不同则返回明确的 Invalid 冲突。应用需要预先对齐运行库环境；平台没有隐式合并 LD_PRELOAD/LD_LIBRARY_PATH。本轮没有增加自动兼容逻辑。

排查时依次查看 Workload 的 Decision.reason、Selected/Reserved/RuntimeReady/Fallback/Degraded Conditions 与 Events，再用 allocationRef 定位 Lease：CapacityChanged 表示已拒绝该次预留，ReservationRetrying 表示取得或回滚结果仍不确定，Releasing 且仍有 finalizer 表示释放尚未确认。具体释放 API 错误目前由控制器错误日志记录；不能仅根据 session=0 或一个 False Condition 判断容量已释放。

```bash
kubectl -n <namespace> describe acceleratorworkload <workload>
kubectl -n <namespace> get acceleratorlease <allocationRef> -o yaml
kubectl -n accelerator-system get lease -l accelerator.qcrs.io/claim-type=remote-endpoint -o wide
kubectl -n accelerator-system logs -l app.kubernetes.io/name=accelerator-control-plane --all-containers --prefix --since=10m
```

## 验证与交付状态

- 8 组新增定向回归通过，包含 12 个分支场景：[最终定向记录](evidence/correctness-review-20261119/regressions-final.log)。
- `make verify`：[通过](evidence/correctness-review-20261119/verify-final.log)。
- `go test -race ./...`：[通过](evidence/correctness-review-20261119/race-final.log)。
- 真实 API Server/etcd envtest，包括 Claim 竞争及旧状态不能撤回 Ready：[通过](evidence/correctness-review-20261119/envtest-final.log)。
- 原有 Python/native HTTP 测试 28/28：[通过](evidence/correctness-review-20261119/python-tests.log)。
- 新控制器镜像：`accelerator-control-plane:arm64-correctness-20261119-v2`；CoreX/MLU server 与 client 镜像不变。
- 新版本 K3s 13 场景：13/13 PASS，执行于 `2026-11-19 13:15:28–13:23:50 UTC`，命名空间 `accelerator-validation-1119131528`。
- 独立复核 PASS：按记录的 verifiedAt 离线重放 11 组数值成功证据；2 个 Ready 后故障按预期 exit=1 且未重放；2 个运行中删除的 Workload、Pod、Lease 均确认不存在。15 个客户端 Pod 及 4 个系统 Pod 的实际 config digest 与包内元数据一致，78 个源码/模块/构建文件哈希未变化。
- 生命周期采样 105 组，其中 Unknown/Available=0 为 23 次、Reserved>0 且 Session=0/Available=0 为 61 次；未发现 Endpoint 对象内部的可用容量冲突。跨对象原子所有权仍由既有 Claim 保证。
- 最终两个 Endpoint 均为 Healthy/Observed、Session=0、Reserved=0、Available=1；所有平台任务终结、无 Endpoint Claim。manager 两副本、CoreX/MLU 各一副本 Ready，原 CoreX DNS 已恢复，临时 ClusterIP Service 已删除。

离线包位于 `dist/k3s-correctness-20261119-v2/`，镜像归档为 95,418,368 字节，SHA256 为 `e266cdb3b6d246f986c0801a72bc9fe64a638069ea6d1d6e2ea2732ff8419587`。[归档和脚本核验](evidence/correctness-review-20261119/delivery.json)、[API Server dry-run diff](evidence/correctness-review-20261119/deployment-plan.diff) 已保存：部署差异只有 manager 镜像及自动递增的 generation。部署前空闲与集群身份检查、回滚脚本语法检查均通过。用户导入镜像后，等待 kubelet 镜像状态刷新，完成滚动部署与实际镜像身份核对，再执行完整矩阵。

当前用户访问主机 containerd socket 仍返回 permission denied，本次镜像由用户在主机通过 sudo 导入。验收与源码快照已另外复制到离线包目录；包内原始 bundle.json 仍记录准备时状态。复现时使用相同包和新的证据目录，原始验收输出保留：

```bash
python3 examples/remote-real-smoke/k3s_suite.py \
  --bundle dist/k3s-correctness-20261119-v2 \
  --evidence docs/evidence/correctness-review-NEW-RUN \
  --disruptions
```

首轮测试 fixture 的 placement order 和 stale RV 已修正；相关试跑日志保留，但不算作生产 Bug 的复现证据。问题判定使用上述有效 fixture 的修复前记录和最终定向记录。
