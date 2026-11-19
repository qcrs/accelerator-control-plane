# 新版 K3s 验收记录（2026-11-19）

后续更新（13:24 UTC）：正确性修复版本 `arm64-correctness-20261119-v2` 已再次完成 13/13 与独立复核，详见 [平台正确性审查](platform-correctness-review.zh-CN.md)。下文保留早先 `arm64-logic-20261119-v3` 的部署与验收记录。

## 当前结果

**真实 Kubernetes 完整矩阵 13/13 PASS，最终资源和证据复核 PASS。** 测试于 `2026-11-19 09:43:57–09:52:19 UTC` 在单 ARM 节点 K3s 上执行；客户端 Pod 无本地 GPU/NPU 设备和 SDK hostPath，计算实际由 CoreX GPU / MLU NPU 服务端完成。

本次提交了 15 个实验 Workload：11 个成功任务都有明确数值日志、exit=0、Released Lease 和 session 清理证据；2 个 Ready 后断服任务按预期 exit=1，保持原分配且未重放；2 个运行任务通过删除验收。竞争场景各包含两个任务，因此场景数为 13。

最终两个 Endpoint 均为 Healthy、Observed、ActiveSessions=0、Reserved=0、Available=1；所有平台任务已终结、没有 Endpoint Claim；两个 server 各一个副本、manager 两个副本全部 Ready。CoreX 原稳定 DNS 已恢复，临时 ClusterIP Service 已删除。

原始结果：[矩阵 summary](evidence/k3s-validation-20261119/full-reliability/summary.json)；独立复核：[audit](evidence/k3s-validation-20261119/full-reliability/audit.json)。复核包含 15 个 Pod 的实际镜像 config digest，以及按记录的 verifiedAt 重放 11 组数值证据。此前失败现场保留，未被新结果覆盖。

## 本次实际变更

- 当前集群控制器为 `accelerator-control-plane:arm64-logic-20261119-v3`，两个副本滚动更新完成。
- 当前集群 CoreX/MLU server 均为 `arm64-observed-v4`。其余 client/workload 镜像仍为 `arm64-observed-v1`。
- CoreX 仍使用 StatefulSet 稳定 Pod DNS；Provider 原样注入域名，没有提前解析为 Pod IP。
- MLU Deployment 使用 Recreate，避免更新时同时运行两个后端实例。
- CRD 采用仓库 kustomize 产物，包含安全的 PodTemplate metadata overlay。
- 原有 default 命名空间的两个已完成样例没有删除或重建。新增测试使用独立命名空间。
- 未推送 Git、未发布镜像到远端仓库、未使用特权调试 Pod 访问主机 containerd 或日志文件。

部署前检查所有 Workload、AcceleratorLease 与 Endpoint Claim；当时没有活动任务或未释放的 Claim。最终部署与回滚文件、快照、七镜像离线包、验收结果和源码哈希已保存到仓库的 `dist/k3s-validation-20261119-reliability/`，复制后的镜像包 SHA256 再次校验通过。`dist/` 已被 Git 忽略，未把大镜像包加入 Git；原临时目录也保留。

## 镜像身份的核对

本机 Docker 使用 OCI image store：`docker image inspect .Id` 对应 manifest digest，不能直接标作 config digest。旧构建报告中同名 `configDigest` 字段存的是 Docker image ID；原始证据保留，本轮修正后续生成器的命名。

本轮从镜像包中的 manifest/config 原始字节分别计算 SHA256，并验证 manifest 引用的 config。本集群 Pod status 返回的是 config digest；最终四个系统 Pod 与实验中 15 个 Pod（含 init 容器）均与可靠性包中的同类 digest 匹配。详见最终 audit 和 `reliability-bundle.json`；`system-image-digests.json` 保留首批部署记录。

新版构建报告使用 `dockerImageID` 和 `manifestDescriptor`；K3s 离线包额外给出从归档推导的 `configDigest`、`manifestDigest`，避免比较不同对象的哈希。

## K3s 日志通道修复

故障时的观测：

1. `kubectl logs` 请求新 Pod 和原有旧 Pod 都返回 `Get https://192.168.31.244:10250/containerLogs/...: EOF`。
2. API Server 的 Node proxy `/healthz` 也返回 ServiceUnavailable / EOF。
3. 使用集群公开 CA、绕过当前终端代理后，直连 kubelet 10250 的 TLS 验证成功，匿名请求返回预期的 HTTP 401；没有关闭证书校验。
4. 清除当前终端 HTTP(S) proxy 后，`kubectl logs` 仍失败。
5. 用户已在主机终端执行只读诊断：K3s 进程启动环境中的 HTTP_PROXY/HTTPS_PROXY（含小写）均指向 `http://127.0.0.1:54321`，没有 NO_PROXY；系统日志记录了 API Server 代理请求 EOF。
6. 同一公开 CA 和 kubelet 地址的对照请求：直连返回 401，显式经过该代理则 TLS 超时。报告见 `kubelet-direct-vs-proxy.json`。

这些证据发现了明确的代理绕过配置缺失。K3s 官方要求 Node 地址列入 NO_PROXY。[代理配置](https://docs.k3s.io/advanced#configuring-an-http-proxy)。补齐配置并重启后，Node proxy 和新旧 Pod 日志均恢复。`/proc/PID/environ` 仅反映启动环境，恢复结论以实际请求和后续矩阵日志为依据。

诊断入口为 `scripts/diagnose-k3s-log-channel.py`。它只读取指定的代理/网络信息和错误摘要，不输出 token、密码或 kubeconfig，不修改配置或重启 K3s。

修复入口为 `scripts/fix-k3s-no-proxy.py`，默认仅打印方案：

```bash
python3 scripts/fix-k3s-no-proxy.py
```

它准备独立 systemd EnvironmentFile 和 drop-in，只设置 NO_PROXY/no_proxy，保留已有绕过条目并加入本机节点地址、Pod CIDR 与集群域名。原有 `k3s.service.env` 和 HTTP(S) proxy 均不改动。配置与回滚命令见 `evidence/k3s-validation-20261119/no-proxy-repair-plan.txt`。

实际生效需要有权限的主机终端执行以下命令，会短暂重启 K3s 控制面。脚本核对原节点 UID，在写入前和重启前检查没有活动任务或 Claim；拒绝覆盖已有同名配置；失败时撤销本次新增文件。

```bash
sudo python3 /home/lvtong/vGPU/vGPU/accelerator-control-plane/scripts/fix-k3s-no-proxy.py --apply
```

用户已执行该修复。K3s 于 `2026-11-19 05:03:07 UTC` 重新启动，Node proxy 与 kubectl logs 验证通过，证据为 `log-channel-repaired.json`。没有继续修改网络模式、证书或 CoreX/MLU 协议。

## 真实验收发现并修正的问题

### MLU server 镜像需要完整的执行依赖

首个 Kubernetes MLU 任务的模块加载返回 Unsupported，Pod exit=77。服务端源码在 native load 前读取 `vector_add.manifest` 并验证 operator ABI，而原镜像只打包了 fatbin。补齐 manifest 后，完整镜像测试进一步返回 `CN_FATBIN_LIBCNBIN_NOT_FOUND`，确认还缺少 driver 通过 dlopen 加载的官方 `libcnbin.so`。

旧 `ldd server` 检查只覆盖静态声明的动态依赖；此前容器客户端连接的是宿主机 server，也没有证明 server 镜像自包含。构建脚本现在预检并成对打包 fatbin/manifest，显式打包 libcnbin 及其依赖，并新增 `scripts/verify-mlu-server-image.py`，直接验证真实 server 镜像的 kernel 及清理。

证据：`full/mlu-auto/`（缺 manifest 的真实失败）、`mlu-image-v2-kernel/`（补 manifest 后发现 libcnbin 缺失）、`mlu-image-v3-kernel/`（完整镜像数值与清理 PASS）。失败的 Kubernetes 任务仍正确保留 readyObserved、没有发生 fallback，Lease 已 Released；这些控制面行为不把应用失败改写成 PASS。

离线包的镜像列表现在从实际部署样例、Provider clientRuntime 和 Workload 镜像派生，不再假定六个组件必须使用同一 tag。后续 server/client 独立发布不会被旧硬编码遗漏。

### 配置消失不能证明从未取得 Claim

旧控制器在当前 Reconcile 无法解析 Provider/Pool 时，直接将 Pending Lease 标为 Failed 并移除 finalizer。但之前一次 Reserve 可能已经取得 Claim，并且回滚失败。新增回归分别删除 Provider 和 Pool，复现了该漏洞。

修复后先根据 Lease 保存的 driver 找到原 Allocator，确认 Release 成功，再结束分配并移除 finalizer。清理仍失败时保留原 Lease、引用和 finalizer；故障恢复并重建控制器后，即使配置已经删除，也能清理原 Claim。此项通过故障注入的控制器回归及全量/竞态测试；不冒称真实 Kubernetes API 故障实验已经完成。

### 管理探测拥塞需要明确响应和正确重试

`full-fixed/` 中的 EOF 请求地址是 `CoreX:50052/v1/observation`，而此前已修复的日志故障地址是 `kubelet:10250/containerLogs`。两条链路各有独立证据，日志通道仍正常。

CoreX 只允许一个 native 设备枚举子进程，旧代码在子进程尚未回收时直接关闭第二个观察连接，造成 EOF。另一个已复现的问题是共享 HTTP parser 在读到请求首行后就回复并关闭，未等待完整 headers；TCP 分段时可能留下未读数据。新增 native C fixture 以分段 TCP 请求复现，修复后通过。

server 现在对探测忙返回 HTTP 503，并读取完整 header；Go Observer 在同一个两秒预算内对 503 和旧服务的提前 EOF 做有限退避重试。超时仍返回 Unknown，不合成 session=0。Allocator 将探测传输错误归为 Transient，保留同一个 Pending Lease、finalizer 和 fallback 预算；恢复后重试原 Endpoint。该错误分类与 Busy/Unhealthy 等明确观测结果分开处理。

重新编译的 CoreX/MLU native server 均通过无卡客户端的真实数值、session admission/cleanup、三路并发 Go Provider 观察和停服检查。MLU v4 完整 server 镜像也通过无卡镜像客户端的 1024 元素 kernel 校验。

### 生命周期状态需要读取已提交结果

旧缓存可在上一轮已经创建 Pod、保存 Bound 后仍返回 Pending，触发再次预留；原 merge patch 也可能用旧快照覆盖已提交的 Ready。两种情况都有回归复现。

生产 Workload Controller 现在将生命周期 Get 和所属 Lease 列举接到 APIReader；Inventory 仍使用缓存。状态写入检查输入 resourceVersion，并使用带 optimistic lock 的 merge patch。API Server/etcd envtest 验证旧状态 patch 必须 Conflict、不能撤回 Ready；缓存延迟回归验证不会对已存在 Pod 重复执行 Reserve。

回滚脚本也按快照恢复 MLU 的真实 Deployment strategy，不再无条件恢复为 RollingUpdate。

## 验收脚本

`scripts/prepare-k3s-validation.py` 准备离线包、快照、部署与回滚入口。它本身不部署、不导入、不调用 sudo；准备时和部署前都拒绝活动任务、未释放 Lease 或残留 Claim，并核对 context 和 Node UID。

`examples/remote-real-smoke/k3s_suite.py` 执行真实 Kubernetes Workload：

| 场景 | 判定内容 | 当前执行状态 |
| --- | --- | --- |
| CoreX/MLU Auto | 无本地扩展资源；客户端无设备节点或 SDK hostPath；选 Remote；明确数值日志；exit=0；Lease Released；Session 清理 | 两种设备均 PASS |
| CoreX/MLU 竞争 | 一个运行，一个无 Pod 排队；worker 执行区间不重叠；都完成数值校验及释放 | 两种设备均 PASS |
| CoreX/MLU 停服后提交 | 观察 Unknown 时新任务 Pending、无 Pod；恢复后同一任务成功 | 两种设备均 PASS |
| CoreX/MLU Ready 后停服 | 已执行数值正确的 kernel 后断服；任务 exit=1；Ready 边界保留；无重放、无新分配 | 两种设备均 PASS |
| CoreX/MLU 删除运行任务 | 已完成一次 kernel 后等待的 Pod 被删除；Workload/finalizer/Claim 清理；Session=0 | 两种设备均 PASS |
| Controller 重启 | 原 Pod 与原 Lease 不变，任务继续成功并回收 | PASS |
| CoreX DNS 重建 | Pod IP `10.42.0.109 → 10.42.0.119`，Endpoint DNS 不变，新任务直接域名执行数值正确 | PASS |
| CoreX ClusterIP | Service IP `10.43.28.184` 转发至 Pod `10.42.0.119`；实际 kernel 数值正确；恢复原地址并删除测试 Service | PASS |

这次已经实际验证当前单后端 CoreX 的 ClusterIP/DNS/NAT 路径可用。StatefulSet/headless Service 可继续提供稳定实例身份，不需要 Provider 提前解析 Pod IP 来绕过 Service。

72 组生命周期采样还记录了 19 次 Unknown/Available=0、38 次 Reserved>0 且 Session=0/Available=0，未发现 Endpoint 对象内部的可用容量语义冲突。采样是状态观察，跨对象原子互斥仍由 Claim 保证。

故障测试只在 `--disruptions` / `--disruptions-only` 下执行，每次操作前拒绝其他命名空间的活动任务或 Claim；`--clusterip-only` 可单独复现 Service 路径。停服场景在 finally 中恢复原副本数。失败时保留终结任务现场，只停止本轮创建且仍活动的测试任务。每次复现必须使用新的 evidence 目录，保留原验收记录。

运行入口（会创建新的测试命名空间和 Workload；故障选项会重启测试 server/controller）：

```bash
python3 examples/remote-real-smoke/k3s_suite.py \
  --bundle dist/k3s-validation-20261119-reliability \
  --evidence docs/evidence/k3s-validation-NEW-RUN \
  --disruptions
```

日志通道在创建任务之前预检；它不可用时直接 FAIL，不用退出码代替数值日志。测试成功的证据还需采集时的新鲜 Endpoint 观察；离线复核应使用记录的验收时间，不能把旧观察当成现在仍健康。

## 已执行的验证

- 七个 ARM64 镜像离线包校验通过。
- CRD、manager、Provider/Endpoint 配置的 API Server dry-run 通过；实际部署及 rollout 通过。
- 四个运行中系统 Pod 的 config digest 与镜像包匹配。
- Python/native HTTP 测试 28/28 通过，包括数值判定反例、占用/Claim/跨集群防护、manifest/config 区分与归档不一致拒绝、业务容器/init 容器镜像身份、代理修复保护、operator bundle 完整性、独立镜像版本、分段请求和回滚策略。
- 首个真实 CoreX Workload 已执行但验收 FAIL，失败证据保留于 `evidence/k3s-validation-20261119/basic/`。
- 日志故障期间，新增预检曾在创建任务前正确拒绝 EOF；修复后日志正常取得。
- NO_PROXY 修复后日志通道恢复、CoreX Auto/竞争真实验收通过。
- MLU v3 完整服务端镜像 + 无卡客户端的 kernel 数值及清理测试通过。
- 配置删除后的 Claim 清理回归、`make verify` 和 `go test -race ./...` 通过。
- 新增探测传输重试、缓存旧状态和 Ready 版本冲突回归通过；更新后的全量与竞态检查通过。
- 新版 envtest（含真实 API Server 状态 CAS）、两个 native server 数值/并发探测、MLU v4 完整镜像数值测试通过。
- 最新 K3s 矩阵 13/13、数值证据离线复核、最终资源与镜像身份复核全部 PASS。

## 对当前逻辑的结论与边界

当前主线合理：可信观察 → 单 Endpoint 选择 → 原子 Claim → 持久化 Lease → Pod 执行 → 确认释放。观察、平台所有权和应用数值结果分别保留事实来源；Session=0 不能替代 Lease 释放，Unknown 不能变成可用，临时探测错误不消耗启动回退，首次 Ready 后失败不自动重放。

这次闭环覆盖单 ARM 节点、每类一个 Endpoint 和两个任务竞争。它未代替物理跨机器验收、规模压测或 MLU native cleanup 错误注入；session 仍是目标 runtime 的聚合计数，不证明整卡其他 native 进程空闲或某 session 的 Lease 身份。

下一阶段适合冻结这版语义与镜像作为基线，再按已有生命周期阶段拆分大 Controller、提取 CoreX/MLU 确实相同的运行库交付代码。性能和更高并发应由单独压测驱动，保持本轮验收契约。

本轮证据目录：`docs/evidence/k3s-validation-20261119/`。可靠性版本已部署；代码、测试和验收记录随本次 Git 提交归档，离线镜像包保留在被忽略的 `dist/` 目录。
