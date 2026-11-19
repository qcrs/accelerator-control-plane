# 运行时观察与测试闭环

后续进展（2026-11-19）：新版已导入并部署现有 K3s，真实矩阵 13/13 与最终资源复核 PASS，包含 CoreX DNS/Pod 重建及普通 ClusterIP 路径，见 [K3s 验收记录](k3s-validation-report.zh-CN.md)。下文保留 11 月 18 日独立硬件实验的范围和复现方式。

## 本轮范围与结果

本轮只修改本地源码、测试和镜像。没有修改现有 K3s 集群、导入 containerd、推送镜像或推送 Git。设备测试运行独立服务进程，只终止本轮创建的进程。

| 验收项 | 结果与证据边界 |
| --- | --- |
| 数值判定 | CoreX 要求 `DRIVER_SMOKE_NUMERICAL=PASS bytes=256`；MLU 要求 `MLU_SMOKE_NUMERICAL=PASS elements=1024` 和 BANG kernel PASS；单独 exit=0 不算通过 |
| 本地无卡、远端有卡 | 同一 ARM 机器，客户端私有 mount namespace 隐藏全部 `/dev` 和 SDK；独立 TCP 服务端访问真实 CoreX GPU/MLU NPU |
| 镜像执行 | 两条无设备 Docker workload 路径通过；独立 runtime 容器先复制库到临时 volume，业务容器只读挂载后执行 |
| 直接 hostname | Provider 已删除预解析；真实客户端直接 `localhost` 解析并执行 kernel；新版 Kubernetes DNS/Pod 重建测试尚未部署执行 |
| Session 观察 | 真实服务端会话计数从 0→1→0，探测不产生数据 session；客户端断开后清理归零 |
| 清理不确定 | CoreX 测试 worker 被终止后 `cleanupErrors>0`、healthy=false；MLU 清理错误由解析器/控制器故障测试覆盖，未对真实 MLU 故意注入 native cleanup 失败 |
| 分配一致性 | 单一独占 Endpoint；不支持 Device 模式或跨 Endpoint 聚合 |
| sidecar | 已验证 restartable init 的阶段累加、应用重叠和 overhead |

完整实验输出在 `docs/evidence/closure-20261118/`，包括独立客户端与容器客户端 stdout/stderr、exit code、前后/活动 session 观察、实际 Go Provider 对 native server 的读取、镜像 SHA256、file/readelf/ldd 和构建日志。

## 观察协议

复用 `contracts.EndpointObserver`。CoreX/MLU 的只读管理接口为 `GET /v1/observation`，与计算协议端口分离，不新增计算 API、不创建数据 session。JSON v1：

```json
{"schemaVersion":1,"driver":"corex-remote","healthy":true,"capacity":1,"activeSessions":0,"cleanupErrors":0}
```

- `healthy`：服务端 native device enumeration 成功，且未发生未确认清理；不等于一次 kernel 数值验收。
- `capacity`：服务端实际枚举的设备数量，必须与 Endpoint 声明数量相符。
- `activeSessions`：从接纳连接直到 native 清理完成的保守计数，包含 handshake 和 cleanup；MLU 附属 callback/IPC 连接也保守计入。它不是精确的任务/Lease 归属证明，也不假设一个 Lease 只能创建一个 session。
- `cleanupErrors`：进程生命周期内锁存的清理错误；不会因当前没有连接就归零。
- CoreX 父进程不初始化 GPU。只读枚举在有超时、最多一个并行探测的短生命周期子进程中完成，数据 session 仍保持原有 worker/broker 模型。
- MLU 复用现有线程会话表、锁及 cleanup 错误计数。

端口由 `COREX_REMOTE_OBSERVATION_PORT` / `MLU_REMOTE_OBSERVATION_PORT` 显式启用；Endpoint `parameters.observationPort` 与之对应。HTTP 客户端禁用代理和重定向，限制响应大小、校验 driver/schema/字段/计数，超时 2 秒。接口沿用当前可信测试网络的信任边界，不提供鉴权/TLS；不能向不可信公网暴露。

## 状态与调度规则

- `status.reserved`：统一从所有未终结 Remote Lease 计算的 **Lease 数量**，包含空 phase、Pending、Reserved、Bound、Releasing。这是账本占用，不代表这些 Lease 都已经取得原子 Claim。
- `status.capacity.total`：Endpoint 声明的设备数量；健康变坏不抹掉声明容量。
- `status.capacity.reserved`：被独占 Lease 保留的设备数量。多设备 Endpoint 被一个 Lease 独占时，可大于 `status.reserved`。
- 原子 `coordination.k8s.io/Lease` Claim：实际平台内所有权的最终保护；状态展示和远端探测不能替代 Claim。取得 Claim 后，Allocator 还会通过同一 Provider 注册表实时复查服务端，确认 Healthy、session=0、容量吻合；Busy/Unknown/超时会回滚本次 Claim，避免前一任务正在清理但缓存状态尚未更新的窗口。
- `activeSessions=nil`：Unknown，绝不是 0。缺少 observer/端口、超时、非法响应时清除旧成功观察；保留 Lease，不因探测失败释放。
- `health`：Healthy / Degraded / Unhealthy / Unknown；新观察最迟一分钟过期，控制器每 15 秒复查。
- `Ready`：配置、远端观察和容量一致性都满足才为 True。Inventory、Pool、分配后 Resolver、创建新 Pod 前复查均不接受缺失/过期/异常观察。
- `available`：仅当观察可信、健康、活动 session=0 且账本占用=0 时显示可分配设备数量；其余为 0。
- 活动 session 存在但没有未终结 Lease：`SessionsConsistent=False / UntrackedSessions`，Warning Event，禁止新分配。
- Bound Lease 存在但没有 session：`SessionsConsistent=Unknown / SessionUnconfirmed`，保留账本占用，等待启动/结束/故障确认；不自动猜测或回收。
- 观察范围是这个 runtime 进程及其 worker；不统计其他 daemon 或直接 native 客户端的整卡占用。Available 是 Endpoint 的可调度视图，不能称为物理 GPU/NPU 全局空闲。
- 聚合计数只能发现不一致，无法证明某 session 属于某 Lease，也不能原子阻止平台外客户端在两次观察间接入。要获得这种保证，必须让平台成为服务端唯一准入方，或后续增加真正的 Lease 身份握手；本轮没有伪造这一能力。
- 不支持 observer 的旧 Provider（包括当前 Cricket）默认不允许新 Remote 分配。旧 Lease 的释放路径不依赖 observer；不会删除 CRD 或擅自迁移正在运行的任务。

观察成功才更新 ObservedAt。条件使用 ObservedGeneration、稳定 Reason 和 LastTransitionTime；Endpoint 自身 status 更新不触发紧循环探测，相同告警不重复发 Event。

## 构建与复现

两个 runtime 仓库均有本地修改，控制面仓库同时保存可审查的补丁 `runtime/patches/`。补丁不是一套假服务：实际服务端源码已修改并在真实硬件上编译运行。基准提交见该目录的 `BASES.json`。对干净副本先 `git apply --check` 再应用；现有开发目录不要重复应用或覆盖用户改动。

```bash
export COREX_ROOT=/home/lvtong/corex-remote-runtime
export MLU_ROOT=/home/lvtong/mlu-remote-runtime
export OUT_DIR=/tmp/accelerator-remote-validation
bash scripts/build-remote-validation.sh
python3 scripts/build-remote-images.py \
  --corex-root "$COREX_ROOT" --mlu-root "$MLU_ROOT" \
  --artifacts "$OUT_DIR" --evidence /tmp/remote-images
python3 scripts/verify-isolated-remote.py \
  --corex-root "$COREX_ROOT" --mlu-root "$MLU_ROOT" \
  --artifacts "$OUT_DIR" --image-tag arm64-observed-v1 \
  --evidence /tmp/remote-evidence
```

构建需要匹配 ARM64 SDK 和两个 runtime 的已生成元数据/MLU client 库/kernel 产物；不需要在镜像构建阶段访问设备。真实执行阶段才需要 server 访问设备。CoreX client 与 server 的变化分别落在两个 runtime 源码仓库；它们不会因为提交控制面仓库就自动发布。

六个新镜像使用独立的 `arm64-observed-v1` tag，避免复用旧 smoke 镜像。业务镜像包含应用和 kernel，运行库经 init-volume 交付，不依赖开发机 hostPath。服务端镜像显式打包解析到的官方 vendor 库及传递依赖；native server 在容器内的 `ldd` 必须没有 not found。

哈希必须按对象类型比较。本机 OCI image store 中 Docker `.Id` 是 manifest digest；早期报告中的 `configDigest` 实际记录了该 ID，不能直接与 K3s 返回的 config digest 比较。后续构建报告改用 `dockerImageID` / `manifestDescriptor`，离线包从归档原始字节分别计算 manifest/config digest；11 月 19 日的同类核对结果见 K3s 验收记录。11 月 18 日尚未导入 K3s，因此该阶段没有宣称两者已对齐。

## 集群验收入口

导入/发布镜像并部署新版本后，再执行 `examples/remote-real-smoke/run.sh`。脚本会修改样例 Workload；本轮未运行该脚本或任何部署动作。

它只有在 Workload Succeeded、Pod exit=0、数值日志存在、Lease Released、Endpoint 最新观察为 session=0/Reserved=0 时才输出 PASS。失败/超时/日志 EOF/缺少 Pod 都返回非零，先保存现场。日志获取失败不能由旧日志或退出码代替。

现有样例中的 server `privileged`/设备 hostPath 仍明确为硬件实验配置，并未因此成为生产安全清单。
