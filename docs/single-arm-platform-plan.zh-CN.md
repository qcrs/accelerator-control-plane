# 单 ARM 机器上的本地/远端异构加速平台实施方案

> 文档状态：实施基线
>
> 目标平台：一台带真实 GPU/NPU 的 ARM64 机器，运行 K3s/Kubernetes
>
> 相关工程：`accelerator-control-plane`、`corex-remote-runtime`、`mlu-remote-runtime`

## 1. 先统一目标

本项目最终要验证并交付的是一套在 Kubernetes/K3s 中运行的异构加速控制面：业务程序不修改源码，平台根据策略选择本地或远端 GPU/NPU，并自动准备相应的运行时。

当前的实际硬件约束是：

```text
一台 ARM64 主机
  ├── 真实 GPU/NPU
  └── K3s/Kubernetes
```

因此当前阶段不需要先寻找第二台 GPU/NPU 机器。可以在同一台 ARM64 主机上同时验证两条路径：

```text
Local 路径：Synthetic Device Plugin / 合成扩展资源
             → 验证控制面本地选择、记账、Pod 注入和回收

Remote 路径：业务 Client Pod
             → TCP
             → Remote Server Pod
             → 当前 ARM 主机上的真实 GPU/NPU
```

这里的 Local 模拟重点是控制面语义，不宣称真实本地设备隔离；Remote 路径可以使用真实设备，只是 Client 和 Server 运行在同一台物理主机上。

## 2. 架构边界

```text
业务用户
  └── AcceleratorWorkload
        ↓
accelerator-control-plane
  ├── Inventory：Node / Pod / Endpoint / Lease
  ├── Policy：LocalOnly / RemoteOnly / Auto
  ├── Allocation：Reserve / Bind / Release
  ├── AcceleratorLease：持久化目标与资源快照
  └── RuntimePlan：容器注入计划
        ↓
  ├── Local Adapter
  │     └── Kubernetes extended resource / Device Plugin
  ├── MLU Remote Adapter
  │     └── ARM64 CNRT/CNDrv remote client
  └── CoreX Remote Adapter
        └── ARM64 CoreX remote client
        ↓
业务 Pod → Client runtime → TCP → Server → 真实设备
```

控制面负责：

- 选择执行模式和目标；
- 预留和回收资源；
- 保存不可变的目标、版本和 Endpoint 绑定；
- 注入动态库、环境变量、volume 和 init container；
- 处理 Pod 启动前的可控失败回退；
- 记录状态、事件和容量。

Remote Runtime 负责：

- CUDA/CNRT/CNDrv API 替换或拦截；
- 客户端与服务端通信；
- 远端设备上下文、内存、队列、模块和 kernel 生命周期；
- 远端资源清理。

控制面不应该直接调用 CUDA、CNRT、CNDrv 或厂商 SDK，也不应该在通用控制器中写 CoreX/MLU 特判。

## 3. 当前机器已经具备的 ARM64 基础

本机已经发现 ARM64 SDK 和产物，不需要把 x86 SDK 伪装成 ARM SDK。

### MLU/Neuware

```text
/usr/local/neuware-3.5.2/
  ├── bin/cncc
  ├── include/
  ├── lib64/libcnrt.so.6.5.2
  └── lib64/libcndrv.so.2.5.2
```

`libcndrv.so.2.5.2` 已确认是 ARM aarch64 ELF。当前 MLU Remote Runtime 的产物也已经是 ARM64：

```text
/home/lvtong/mlu-remote-runtime/build/shim/libcnrt.so
/home/lvtong/mlu-remote-runtime/build/libremote_cnrt.so
```

### CoreX

```text
/usr/local/corex-4.4.0/
  ├── bin/
  ├── include/
  └── lib64/
```

当前 CoreX Remote Runtime Server：

```text
/home/lvtong/corex-remote-runtime/dist/bin/runtime_server
```

已确认是 ARM64 ELF。

这意味着本阶段可以直接围绕 ARM64 构建 Server、Client 和业务镜像。已有的 x86 镜像仍可保留给 x86 节点或历史环境，但不能作为当前 ARM 真实设备 Server 的基础。

## 4. 镜像与容器的正确理解

镜像是包含程序、动态库、基础系统和启动命令的打包物；容器是镜像启动后的进程实例。镜像不会把 x86 二进制转换成 ARM 二进制。

本方案需要区分三类镜像：

### 4.1 ARM64 Remote Server 镜像

运行在真实 GPU/NPU 所在的 ARM 节点上，包含：

- ARM64 基础系统；
- ARM64 厂商 SDK/Driver 用户态库；
- `corex-remote-runtime-server` 或 `mlu-remote-server`；
- 启动、健康检查和优雅退出脚本。

Server 容器需要通过设备插件或受控的设备挂载访问真实设备。宿主机内核驱动、用户态 SDK、设备型号和 runtime 版本必须匹配。

### 4.2 ARM64 Remote Client 镜像

运行在业务 Pod 的 init container 中，只提供远端客户端动态库和必要的系统依赖：

- MLU：`libcnrt.so` shim、`libcndrv.so` shim、`libremote_cnrt.so`；
- CoreX：`libcorex_remote_cudart.so`、`libcorex_remote_cuda.so`、Driver shim；
- 不应默认携带本地设备节点或完整编译工具链。

init container 将库复制到 `EmptyDir`，业务容器通过 `LD_LIBRARY_PATH`/`LD_PRELOAD` 使用。这样业务镜像不需要内置具体远端 runtime。

### 4.3 ARM64 业务镜像

业务源码可以不变，但业务程序、基础镜像和动态依赖必须是 ARM64。若现有业务只有 x86 二进制，需要重新编译或重新制作 ARM64 镜像；`LD_PRELOAD` 不能把 ARM64 库注入 x86 进程。

## 5. 单机实验拓扑

### 5.1 RemoteOnly：真实远端数据面

```text
K3s
  ├── mlu/corex-server Pod
  │     └── 真实 ARM GPU/NPU
  └── AcceleratorWorkload Pod
        ├── runtime-init
        └── ARM64 业务容器
              └── TCP → server Pod
```

Server 与 Client 可以在同一物理机，但必须使用独立进程、独立容器和明确 TCP 地址。这样可以验证协议、容器注入、session 生命周期和真实设备执行。

### 5.2 LocalOnly：合成本地资源

在同一 K3s 节点部署 Synthetic Device Plugin，发布测试资源，例如：

```text
example.com/synthetic-gpu
example.com/synthetic-mlu
```

Local Adapter 只做 Kubernetes 扩展资源请求、Node 约束和 Lease 记账。它验证控制面流程，不验证真实设备驱动。

### 5.3 Auto：同机双候选

同时配置：

```text
Local Pool：Synthetic Device Plugin
Remote Pool：真实 ARM Remote Endpoint
```

按顺序验证：

1. LocalFirst 选择本地合成资源；
2. Local 资源不足后选择 Remote；
3. Local Pod 未 Ready 且 runtime/调度失败时回退 Remote；
4. Remote 连接或 runtime init 失败时回退 Local；
5. Pod 曾经 Ready 后不再自动换设备重放。

## 6. 当前代码中已经具备的能力

`accelerator-control-plane` 不是空壳，当前已有较完整的控制面骨架：

- Provider、Pool、Endpoint、Workload、Lease 五类 CRD；
- Local/Remote/Auto 选择策略；
- Local extended resource 的 Quantity 精确记账；
- Runtime-agnostic `ExecutionAdapter`；
- Provider 与 Allocator 的配对注册；
- 远端 Endpoint 的 API Server 原子 claim；
- 不可变 Lease target 和 Endpoint binding snapshot；
- Pod 注入的幂等和冲突检查；
- 首次 Ready 前的有限回退；
- Workload/Lease/Pod 的释放路径；
- envtest、单元测试和 Cricket Remote E2E 基线。

这些能力可以复用于 CoreX 和 MLU，不需要为每一个 runtime 重写控制器。

## 7. 当前代码需要重点检查和改造的地方

下面分为确定的边界问题、接入前必须补齐的问题和需要实验确认的问题。

### 7.1 适配器仍然过于偏向 Cricket

当前控制器注册的是本地 Device Plugin 和 Cricket。CoreX/MLU 接入前需要：

- 为每个 runtime 新增独立 Provider/Allocator；
- 把 runtime image、source path、mount path、架构、库模式、版本和协议参数纳入 Provider；
- 将 endpoint 的 `runtime.driver`、`runtime.version`、`apiVersion` 与 client image 的实际版本校验绑定；
- 禁止把 Cricket 的 `rpcID`、`LD_PRELOAD`、固定路径和环境变量写入通用层。

### 7.2 NPU 已进入 API，但不是已交付的数据面

API 支持 `DeviceKindNPU`，但当前本地 Provider 仍只是通用扩展资源语义；真实 MLU Device Plugin、设备发现、设备隔离和本地 NPU 验收尚未成为控制面的事实来源。

应明确区分：

```text
控制面可以表达 NPU
≠
当前已完成真实 NPU 本地分配
```

第一阶段可先支持 Remote MLU 和 Synthetic Local MLU，再接入真实 MLU Device Plugin。

### 7.3 远端 Device 级分配不能提前打开

CRD 定义了 `RemoteAllocationModeDevice`，但当前 RemoteAllocator 明确只支持独占 Endpoint。CoreX/MLU 接入时应继续拒绝 Device 模式，直到满足：

- server 能按 session 隐藏未分配设备；
- allocator 能原子分配设备或 slice；
- runtime 能隔离不同租户；
- Lease 能记录不可变 device binding；
- 有并发隔离测试。

### 7.4 Endpoint Ready 目前仍偏静态

配置 Controller 可以发布 `Configured`，但 live `Ready` 需要独立的健康观察者。真实接入时必须增加：

- TCP/RPC/协议握手探针；
- runtime version 和 server capability 探针；
- `lastProbeTime` 与过期处理；
- server 断开后的 Endpoint 降级；
- 重新 Ready 后触发等待中的 Workload。

静态 Endpoint 配置正确不能代表远端 server 可用。

### 7.5 本地预留不是 kube-scheduler 的原子所有权

当前本地 Lease 是控制面提前记账，最终设备分配仍由 kube-scheduler、kubelet 和 Device Plugin 决定。并发情况下可能出现控制面认为可用、调度器最终拒绝的情况。

第一阶段应把这种情况作为可恢复的 `Unschedulable/CapacityChanged`，并验证：

- 旧 Pod 未确认删除前不释放；
- 创建结果未知时不提前释放；
- 重试不会选择不同目标覆盖同一个 Lease；
- controller 重启后可恢复。

生产级原子预留若不足，后续再评估 DRA/Reservation，不要在当前阶段伪造强一致。

### 7.6 Runtime 版本与镜像 digest 需要进入事实模型

目前版本主要通过 Provider/Endpoint 字段匹配。接入 ARM 镜像后必须额外记录：

- client image digest；
- server image digest；
- SDK 版本；
- runtime commit；
- CPU architecture；
- 协议/API 版本；
- 设备型号和驱动版本。

这些值应进入 Lease binding snapshot 或 Workload status，确保后续排障能还原实际运行组合。

### 7.7 错误分类还要覆盖 runtime 连接和 ABI 错误

应区分：

- ImagePullBackOff；
- runtime init 复制失败；
- 动态库加载失败；
- ABI/架构不匹配；
- Endpoint 不可达；
- 协议握手失败；
- server session 被动断开；
- 业务容器自身失败。

只有平台和 runtime 初始化类错误才允许在首次 Ready 前回退；业务进程错误不能默认触发换设备重跑。

### 7.8 安全能力暂时不能作为生产承诺

现有 Cricket 和两个 remote runtime 的验证边界都不等于生产安全能力。接入 CoreX/MLU 前需要明确：

- 当前是否无 TLS；
- 是否无认证；
- Endpoint 是否只能在可信内网使用；
- 是否需要 NetworkPolicy；
- 是否需要 Secret 引用；
- 日志和 Event 是否会泄露地址或凭据。

控制面应保留 TLS/Auth 能力声明，但 Adapter 不支持时必须在配置阶段拒绝，而不是静默忽略。

## 8. 需要新增的控制面模型

建议在 `ClientRuntimeSpec` 或等价 API 中增加以下信息：

```yaml
clientRuntime:
  image: registry.example.com/remote/mlu-client:v1
  imageDigest: sha256:...
  architecture: arm64
  runtimeVersion: neuware-3.5.2
  protocolVersion: mlu-remote-v1
  libraryMode: preload
  sourcePath: /opt/remote-runtime/lib
  mountPath: /opt/accelerator/runtime
```

Provider/Endpoint 校验应检查：

```text
业务 Pod 架构 == client image 架构
client runtime 版本 == Endpoint server runtime 版本
protocol version 可协商
设备类型和 resource form 匹配
```

当前第一版可以只支持 `arm64 + dynamic + preload + WholeDevice + ExclusiveEndpoint`，不要过早开放所有组合。

## 9. 镜像制作计划

### 9.1 MLU ARM64 镜像

输入：

```text
/usr/local/neuware-3.5.2
/home/lvtong/mlu-remote-runtime/build
```

产出：

```text
mlu-remote-server:arm64
mlu-remote-client:arm64
mlu-workload-base:arm64（可选）
```

Server 镜像需要最小化，只保留运行期库和 server；编译器、头文件和完整 samples 不应进入生产 Server 镜像。Client 镜像只保留经过验证的 Shim/Client 库。

### 9.2 CoreX ARM64 镜像

输入：

```text
/usr/local/corex-4.4.0
/home/lvtong/corex-remote-runtime/dist
```

产出：

```text
corex-remote-server:arm64
corex-remote-client:arm64
corex-workload:arm64-driver-smoke
```

Driver smoke 镜像由 `corex-remote-runtime/scripts/build-workload-image.sh` 构建，必须同时包含 `driver_smoke_app` 和 `driver_kernel.cubin`；Kubernetes 样例必须把这两个文件配对使用。

同样区分 build image 和 runtime image：

```text
build image：编译器、头文件、静态库、生成工具
runtime image：动态库、server/client、系统依赖
```

不要把完整编译 SDK 默认塞进业务运行镜像。

### 9.3 镜像验收

每个镜像构建后必须执行：

```bash
file /path/to/binary
readelf -d /path/to/library
ldd /path/to/binary
```

验证：

- 所有 ELF 均为 AArch64；
- 无意外链接 x86 库；
- SONAME 和 RPATH 正确；
- 非 root 用户可运行 Client；
- Server 能访问设备；
- Client 不访问本地设备节点；
- 版本和协议信息可查询。

## 10. 实验矩阵

### A. 基础运行时

| 实验 | 目的 | 通过标准 |
|---|---|---|
| ARM Server 启动 | 验证设备 SDK、权限和 server | Ready，版本正确 |
| ARM Client 连接 | 验证网络和握手 | 建立 session |
| 基础内存 | 验证 H2D/D2H/分配释放 | 结果与 native 一致 |
| Kernel | 验证真实执行 | CPU reference 一致 |
| 多线程/异步 | 验证并发边界 | 无资源泄漏、顺序正确 |
| 断连清理 | 验证 session 回收 | server 资源归零 |

### B. 容器无感

| 实验 | 目的 |
|---|---|
| 原业务源码不变 | 验证 API 无需修改 |
| 业务镜像不内置 runtime | 验证 init 注入 |
| 业务容器隐藏 SDK | 验证 Client 依赖闭包 |
| 业务容器隐藏 `/dev` | 验证真正走远端 |
| 动态链接 | 验证 `LD_LIBRARY_PATH`/`LD_PRELOAD` |
| 业务容器退出 | 验证回收 |

### C. 控制面

| 实验 | 目的 |
|---|---|
| RemoteOnly | 选择 Endpoint、创建 Pod、执行、释放 |
| LocalOnly | Synthetic Device Plugin 本地流程 |
| Auto LocalFirst | 本地可用时优先本地 |
| Auto fallback | 本地启动失败切远端 |
| Remote fallback | 远端失败切本地 |
| 并发抢占 | 仅一个 Workload 获得独占 Endpoint |
| Controller 重启 | 不丢 Lease、attempt 和 binding |
| Pod 删除/断电 | 最终回收远端 session |

### D. K3s

- containerd 下运行 ARM64 镜像；
- 非 root Controller；
- ServiceAccount/RBAC；
- K3s 网络到远端 server；
- Pod 重启和节点重启；
- ARM64 节点标签和调度；
- CNI MTU、DNS、NetworkPolicy；
- 镜像 digest 和离线导入。

## 11. 分阶段执行顺序

### M0：代码和模型收敛

1. 保留现有 CRD 和状态机，但补充 architecture、image digest、protocol/runtime version；
2. 把 Cricket 特定字段移到 Cricket Adapter；
3. 明确 Remote Device 分配暂不支持；
4. 补充错误分类和事件；
5. 建立 ARM64 编译和镜像构建脚本；
6. 将 Synthetic Device Plugin 作为测试资产，不混入生产 Adapter。

### M1：单机 MLU Remote

1. 构建 `mlu-remote-server:arm64`；
2. 构建 `mlu-remote-client:arm64`；
3. 手工运行 Client → Server → 真实 MLU；
4. 部署到 K3s；
5. 接入 `MluRemoteAdapter`；
6. 完成 RemoteOnly、回收和失败测试。

### M2：单机 CoreX Remote

按 M1 复制流程，接入 `CoreXRemoteAdapter`，但单独维护 CUDA/CoreX API 兼容矩阵。

### M3：Local Synthetic + Auto

加入合成本地资源，验证双路径选择和回退。

### M4：真实本地资源

接入真实 Device Plugin，验证本地 GPU/NPU 设备、权限、可见性和隔离。此阶段之前不对“本地真实设备能力”做生产承诺。

### M5：生产化

- Endpoint 主动探针；
- TLS/认证或可信网络隔离；
- metrics、告警和 SLO；
- quota 和并发控制；
- leader 切换、网络分区和升级回滚；
- 兼容矩阵和发布签名。

## 12. 当前验证限制

本次检查发现当前环境的 shell 中没有 `go` 命令，因此本轮没有重新执行 `go test ./...`；仓库历史交付记录中的测试结果不能替代当前环境复跑。后续执行代码修改前应先恢复 Go 工具链，再运行：

```bash
go test ./...
go vet ./...
go build -trimpath -o bin/manager ./cmd/manager
```

在 Go 工具链恢复前，可以继续完成：

- ARM64 ELF 检查；
- SDK 和 runtime 依赖盘点；
- Dockerfile 和镜像构建设计；
- CRD、Adapter 和实验脚本静态审阅。

## 13. 最终验收标准

只有以下条件同时满足，才可以称为第一版目标完成：

1. ARM64 MLU 或 CoreX Server 能在当前真实设备上启动；
2. ARM64 Client 能在不挂本地设备的业务容器中加载；
3. 业务源码不修改，至少一个真实工作负载数值正确；
4. RemoteOnly Workload 能由控制面创建、运行和释放；
5. Synthetic LocalOnly 能完成资源选择和 Pod 注入；
6. Auto 能按本地优先和失败回退工作；
7. Controller 重启、Pod 删除和 server 断连不会泄漏 Lease/session；
8. 运行组合记录了镜像 digest、SDK、runtime、协议和设备版本；
9. 所有“已支持”能力都有可复现实验记录；
10. 未支持的架构、静态链接、远端共享和安全能力在 API 或文档中明确拒绝。

## 14. 执行结论

正确的实施顺序不是先把所有 x86 镜像迁移到 ARM，而是：

```text
盘点 ARM SDK
  → 构建 ARM64 Server/Client runtime 镜像
  → 单机真实 Remote 验证
  → Synthetic Local 验证
  → 接入 Control Plane
  → Auto/Fallback
  → 真实本地 Device Plugin
  → 生产化
```

当前最重要的下一步是完成 MLU ARM64 Server/Client 镜像和最小 K3s RemoteOnly 验证，然后再修改控制面接入第二个 Adapter。这样可以先证明数据面闭环，再让控制面承担选择和回收责任。

## 15. 编译镜像、运行镜像和设备镜像必须分开

“镜像”不是一个概念。为了支持无卡 ARM 编译节点、带卡 ARM Server 和不带设备的业务 Pod，至少需要四种镜像。

### 15.1 ARM64 编译镜像

编译镜像只负责生成业务程序或 remote runtime，不负责运行真实设备程序，也不需要挂载 `/dev`。

MLU 编译镜像应包含：

```text
ARM64 Ubuntu 基础系统
/usr/local/neuware-3.5.2/bin/cncc
/usr/local/neuware-3.5.2/include
/usr/local/neuware-3.5.2/lib64（编译链接所需库）
必要的 clang、gcc、cmake、make、python
```

CoreX 编译镜像应包含：

```text
ARM64 Ubuntu 基础系统
/usr/local/corex-4.4.0/bin/clang++
/usr/local/corex-4.4.0/include
/usr/local/corex-4.4.0/lib64（编译链接所需库）
必要的 gcc、cmake、make、python
```

编译镜像的约束：

- 不需要真实 GPU/NPU；
- 不需要 `/dev` 设备；
- 不运行 `cncc` 生成的程序；
- 不把编译器镜像直接作为生产业务镜像；
- 编译产物必须标记架构、SDK 版本、runtime commit 和 ABI。

因此，一台没有卡的 ARM 机器也可以作为 ARM64 编译节点，只要有 ARM64 SDK、头文件、编译器和链接库。

### 15.2 ARM64 业务运行镜像

业务运行镜像只包含：

- ARM64 基础系统；
- 业务程序及其普通依赖；
- 不包含完整 `cncc`、`clang++` 和 SDK samples；
- 不强依赖本地 GPU/NPU。

编译阶段使用编译镜像，运行阶段使用业务镜像。二者通过多阶段构建或构建产物传递连接：

```text
compiler image → application binary → workload image
```

### 15.3 ARM64 Remote Client 镜像

Client 镜像只包含注入业务容器所需的远端动态库和配置工具。它不应包含完整编译工具链，也不应默认访问设备。

MLU Client 的最小集合来自：

```text
mlu-remote-runtime/build/shim/libcnrt.so
mlu-remote-runtime/build/driver-shim/libcndrv.so
mlu-remote-runtime/build/libremote_cnrt.so
```

CoreX Client 的最小集合来自：

```text
corex-remote-runtime/dist/lib/libcorex_remote_cudart.so*
corex-remote-runtime/dist/lib/libcorex_remote_cuda.so*
corex-remote-runtime/dist/lib/libcuda.so*
```

### 15.4 ARM64 Remote Server 镜像

Server 镜像运行真实设备，需要：

- ARM64 remote server；
- ARM64 厂商 SDK/Driver 用户态依赖；
- 设备访问权限；
- server 的健康检查和优雅退出；
- 与宿主内核驱动、设备型号和 SDK 版本匹配。

Server 镜像可以不安装完整编译器，但必须包含 server 的动态依赖。编译器和头文件应留在编译镜像中。

## 16. 无卡 ARM 编译节点的运行方式

无卡 ARM 编译节点可以执行：

```text
编译源码
生成 ARM64 ELF
检查动态依赖
制作 workload/client/server 镜像
运行协议级和控制面级测试
```

无卡 ARM 编译节点不能执行：

```text
真实 MLU/GPU kernel
真实设备内存分配
真实设备隔离
真实 Driver 行为
```

因此实验要把“构建成功”“协议成功”“真实设备执行成功”分成三个结果，不能用编译通过替代硬件验收。

## 17. 卡切分和本地/远端模拟

### 17.1 控制面切分模拟

在没有真实本地设备或暂时不接入真实 Device Plugin 时，可以用 Synthetic Device Plugin 发布多个逻辑资源：

```text
example.com/gpu-whole: 1
example.com/gpu-slice: 4
example.com/mlu-share: 8
```

然后配置多个 Pool：

```text
LocalWholePool：WholeDevice，容量 1
LocalSlicePool：Shared，容量 4
RemotePool：Remote，Endpoint 独占
```

这样可以验证：

- 资源形态匹配；
- count 和 Quantity 记账；
- 多个 Workload 争用切分份额；
- LocalFirst 和 RemoteFallback；
- Lease 释放后容量恢复。

但 Synthetic Device Plugin 只能模拟 Kubernetes 资源语义，不能模拟真实显存、算力隔离或硬件上下文隔离。

### 17.2 真实卡切分

真实切分需要下层设备系统负责：

- NVIDIA MIG、HAMi 或其他 Device Plugin；
- MLU 的设备插件、配额和隔离机制；
- 物理 device UUID、profile、显存和算力限制。

控制面只读取已经发布的资源，不能自行声称已经创建了 MIG 或实现了 MLU 隔离。只有真实 Device Plugin 和数据面验收通过后，才能把 `Shared`/`MIG` 标记为生产能力。

### 17.3 同机同时模拟 Local 和 Remote

一台 ARM 机器可以配置：

```text
Local：Synthetic Device Plugin，模拟本地 GPU/MLU 资源
Remote：Server Pod，使用真实 ARM GPU/MLU
Client：业务 Pod，通过 TCP 调用 Server
```

通过降低 Synthetic Local 容量，可以强制测试：

```text
本地有资源 → Local
本地资源不足 → Remote
本地启动失败 → Remote fallback
远端不可用 → Local fallback
```

## 18. 远端空闲和可用性不能只靠缓存

远端“是否空闲”至少分为三层：

### 18.1 配置可用

Endpoint 声明：

- 地址；
- runtime driver/version；
- 可见设备；
- 设备类型、型号和显存；
- allocation mode。

这只能说明结构合法，不说明 server 在线。

### 18.2 Server 在线和能力可用

Remote Server 应提供轻量握手或健康接口，至少返回：

```text
serverId
runtimeVersion
protocolVersion
architecture
device inventory
healthy devices
active sessions
capacity/quota
generation/epoch
```

Endpoint Observer 定期探测并更新：

```text
status.configured
status.ready
status.lastProbeTime
status.observedGeneration
status.capabilities
```

探针超时或版本不匹配时，Endpoint 进入不可调度，新的 Workload 不再选择它。

### 18.3 最终资源所有权

“空闲”不能只依赖 Controller cache。正确顺序是：

```text
缓存判断候选
  → 创建 AcceleratorLease
  → API Server coordination Lease 原子 claim
  → Server Reserve/Bind session
  → Pod 创建
```

多个 Workload 同时抢同一 Endpoint 时，只有一个 claim 成功。当前 `RemoteAllocator` 已有这种 API Server 原子 claim 基础，但 CoreX/MLU Adapter 还需要将 claim 与 server session 建立绑定。

## 19. 推荐的远端控制协议

Server 和控制面不应只通过“端口能连接”判断空闲，建议实现以下控制消息：

```text
HELLO
  clientVersion
  protocolVersion
  architecture
  runtimeDriver

CAPABILITIES
  deviceType
  devices
  resourceForms
  maxSessions
  supportsDeviceAllocation

RESERVE
  leaseUID
  endpoint
  deviceIDs / count
  ttl

RESERVE_ACK
  sessionID
  leaseUID
  expiresAt
  serverEpoch

HEARTBEAT
  sessionID
  leaseUID
  sequence

RELEASE
  sessionID
  leaseUID

GOODBYE / DRAIN
```

对于当前只支持独占 Endpoint 的 CoreX/MLU Server，`RESERVE` 可以先按 Endpoint 粒度实现。后续支持设备级或份额级分配时，再把 `deviceIDs` 和 quota 纳入原子协议。

控制面与 Server 的职责应分开：

- Endpoint Observer 负责健康和能力状态；
- AcceleratorLease 负责 Kubernetes 侧声明和恢复事实；
- Server session 负责运行期资源；
- Adapter 负责把二者按 Lease UID 幂等绑定。

## 20. 远端资源状态机

建议 Server 侧和控制面侧共同遵守：

```text
Configured
  → Probing
  → Ready
  → Reserved
  → Bound
  → Running
  → Releasing
  → Free
```

异常路径：

```text
Ready → Unhealthy
Reserved → Expired
Bound → Disconnected
Running → Orphaned → Reclaimed
```

必须定义：

- TTL 和续租；
- server 重启后的 epoch 变化；
- client 断连后的回收时间；
- controller 重启后的重绑定；
- stale session 防止误释放新租户资源。

## 21. 这几个问题的最终答案

1. **编译镜像需要安装 `cncc`、`clang++` 和 ARM64 SDK 吗？**
   
   需要。编译镜像负责生成 ARM64 业务程序、kernel image 和 remote runtime；它不需要真实卡，也不负责执行。

2. **Server 镜像需要完整编译器吗？**
   
   不需要。Server 镜像只保留运行所需的 ARM64 SDK/Driver 用户态库、remote server 和依赖。

3. **Client 镜像需要完整 SDK 吗？**
   
   通常不需要。只带经验证的 client shim、协议库和动态依赖，业务容器不挂本地设备。

4. **一台 ARM 机器能否模拟本地和远端？**
   
   可以。Remote 使用真实 ARM GPU/NPU；Local 使用 Synthetic Device Plugin 模拟资源语义。

5. **能否模拟卡切分？**
   
   可以模拟控制面容量、份额、Lease 和调度；不能用合成资源替代真实 MIG/共享硬件隔离验收。

6. **远端如何知道是否空闲？**
   
   通过配置 inventory、周期性健康/能力探针、API Server 原子 claim 和 server 侧 RESERVE/HEARTBEAT/RELEASE session 四层共同决定，不能只读缓存或只 ping 端口。

## 22. 第一阶段只制作两个镜像

虽然长期平台可以拆成 Builder、Business、Client、Server 四类镜像，但当前单 ARM 机器的第一阶段不应先引入四套制品。先制作两个 ARM64 镜像，完成真实 Remote 数据面和控制面接入。

### 22.1 `remote-client:arm64`

这个镜像包含：

- MLU 或 CoreX 的远端 Client 动态库；
- 必要的系统运行库；
- 版本和协议元数据；
- 一个用于 init container 的复制入口。

它不包含：

- 完整 `cncc`/`clang++` 编译器；
- 完整 SDK headers 和 samples；
- 本地 GPU/NPU 设备节点；
- Remote Server。

第一阶段可以把一个测试业务程序与 Client 放在同一个开发/验证镜像中，但生产业务仍应使用 init container 注入 Client。推荐的验证 Pod 结构是：

```text
remote-client:arm64 init container
  → EmptyDir
  → ARM64 business container
  → LD_LIBRARY_PATH / LD_PRELOAD
```

### 22.2 `remote-server:arm64`

这个镜像包含：

- MLU 或 CoreX Remote Server；
- 真实设备所需的 ARM64 SDK/Driver 用户态运行库；
- server 配置、健康检查和优雅退出脚本。

它运行在当前带真实 GPU/NPU 的 ARM 主机上，需要受控地访问设备节点。Server 不与业务进程合并，使用独立 Pod/进程和 TCP Service。

### 22.3 第一阶段可以使用开发合并镜像

为了快速验证，允许使用一个临时开发镜像同时包含：

```text
ARM64 compiler + SDK + remote client + test workload
```

该镜像只用于：

- 编译测试程序；
- 启动测试 Client；
- 验证 Client → Server → 真实设备；
- 生成兼容性证据。

它不应直接作为生产业务镜像，因为包含编译器和完整 SDK，体积、权限和版本边界都不适合生产。

### 22.4 后续再拆成四类镜像

当两个镜像的 RemoteOnly 和控制面流程通过后，再拆成：

```text
mlu/corex-builder:arm64
业务镜像:arm64
mlu/corex-client:arm64
mlu/corex-server:arm64
```

拆分的收益是：

- 业务镜像不再携带编译器和完整 SDK；
- Client 可以独立升级而不重建业务镜像；
- 同一业务可以按策略注入 MLU、CoreX 或其他 Client；
- Local/Remote 切换只改变 RuntimePlan，不改变业务镜像；
- Client、Server、业务版本可以分别记录和回滚。

当前阶段的执行优先级明确为：

```text
先做 remote-client:arm64 + remote-server:arm64
再做单机 RemoteOnly
再接入 accelerator-control-plane
最后再拆 Builder/Business/Client 四类镜像

## 23. 当前环境盘点与下一步

截至本轮检查，当前机器状态如下：

```text
架构：aarch64
Docker：已安装，Docker 29.1.3，linux/arm64
containerd：已运行
MLU：MLU370-S4 可见，cnmon 可正常读取
Neuware：/usr/local/neuware-3.5.2 已存在
CoreX：/usr/local/corex-4.4.0 已存在
Go：当前 shell 未安装
kubectl：当前 shell 未安装
k3s：当前 shell 未安装
```

Docker daemon 已运行，但当前用户访问 `/var/run/docker.sock` 返回 permission denied。需要把当前用户加入 `docker` 组，或使用受控的 root/rootless Docker 方式；不要把 Docker socket 直接暴露给业务容器。

### 23.1 为什么控制面使用 Go

`accelerator-control-plane` 是 Kubernetes Controller，不是 GPU/NPU runtime。它使用 Go 的原因是：

- Kubernetes API、CRD、watch、reconcile 和 RBAC 生态以 Go 为主；
- `controller-runtime` 提供 leader election、cache、client、status 和事件机制；
- Controller 需要长期运行、并发处理和可预测的静态二进制；
- Go 代码负责选择、Lease 和 Pod 生命周期，不负责实现 CUDA/CNRT/CNDrv。

因此需要 Go 的是控制面 manager；MLU/CoreX 的编译和运行仍使用各自的 C/C++、CNCC 或 CoreX 工具链。两套工具链职责不同。

### 23.2 环境配置顺序

建议按以下顺序配置，不要先改控制器：

1. 配置 Docker 权限并验证 ARM64 基础镜像构建；
2. 安装 `kubectl` 和 K3s，确认 ARM 节点 Ready；
3. 配置 MLU 设备权限、Neuware 环境和 cnmon；
4. 用当前 SDK 构建 MLU Remote Server/Client 两个镜像；
5. 在 Docker 中完成 Client → Server → 真实 MLU 的单机测试；
6. 安装 Go，运行控制面单元测试和构建；
7. 将 MLU Endpoint/Provider/Pool 接入 K3s；
8. 增加 Synthetic Local Device Plugin，测试 Auto 和 fallback；
9. 再制作 CoreX 镜像和 CoreX Adapter。

### 23.3 第一批镜像构建原则

第一批只构建：

```text
remote-client:arm64
remote-server:arm64
```

可以暂时用一个包含编译器和测试程序的开发镜像帮助构建，但不把它作为平台交付镜像。所有镜像构建都显式指定：

```bash
docker build --platform linux/arm64 ...
```

构建后必须使用 `file`、`ldd`、`readelf -d` 检查 ELF 架构、SONAME、RUNPATH 和动态依赖。

## 24. 当前单机 ARM 实测结果

本轮已经在 `server310p` 上完成以下真实链路：

```text
ARM64 MLU Server 镜像
  → K3s Pod
  → HostPath 设备
  → MLU370-S4

ARM64 Client/Workload Pod
  → Service mlu-remote-server:50051
  → MLU Server
  → 真实 MLU
```

实测结果：

- `mlu-remote-server:arm64` 在 Docker 中识别 `MLU370-S4`，设备数为 1，显存约 24 GiB；
- K3s Server Pod 使用 `mlu-remote-server:arm64` 成功运行；
- K3s Client smoke Pod 通过 Service 建立 HELLO，退出码为 0；
- 基础 H2D/D2D/D2H 内存路径通过；
- `AcceleratorProvider` 的 `driver: mlu-remote` 配置状态为 Ready；
- `AcceleratorPool` capacity 为 total=1、available=1；
- `AcceleratorEndpoint` Configured 为 True；
- `AcceleratorWorkload` 选择 Remote、创建 Pod、注入 Client init container、运行成功并释放 Lease；
- Workload Pod 中可观察到 `MLU_REMOTE_HOST`、`MLU_REMOTE_PORT`、`LD_PRELOAD`、`LD_LIBRARY_PATH` 和 `accelerator-runtime-setup`；
- `AcceleratorLease` 最终进入 `Released`。

另外，临时发布 `example.com/synthetic-mlu: 1` 后，`LocalOnly` 和 `Auto` 均已通过：

- `LocalOnly` 选择 `Local`、绑定 `server310p`、注入本地 requests/limits，Lease Released；
- `Auto` 在 Local 与 Remote 同时可用时按 LocalFirst 选择本地，Lease Released；
- 测试结束后 Node label、capacity/allocatable、Provider、Pool 和 Workload 均已清理。

本轮 Server K3s 清单暂时使用 `privileged: true`，因为目标集群尚未部署 MLU Device Plugin。它只适合单机实验；生产部署必须改为 Device Plugin/DRA 提供设备分配，并收紧 Pod capability、seccomp 和 hostPath 权限。

当前 smoke workload 使用已编译的 MLU 测试程序，CNNL 扩展仍按 runtime 台账标记为 `GROUND_TRUTH_BLOCKED`，不应把该项当作基础 CNRT 远程链路失败。

CoreX 的 Remote 链路与设备后端需要分开判断：

- 2026-11-08 13:03-13:10，M41 在源码快照 `57721ac3b64bb5bb3598ce1611d2087304339b2a` 上，native Driver smoke 以及隔离客户端经 veth/TCP/CRX9 到 CoreX 的 Driver、Runtime/Driver interop、LLM Driver 测试均通过；Remote 客户端没有 `/dev`，服务端完成了数值校验和资源清理。
- 2026-11-08 14:10:37，内核记录 Iluvatar `0000:81:00.0` 的 `XID: 24 mmu page fault`。15:03 的 M45 使用同一源码快照和正确的 `driver_kernel.cubin` 运行 native Driver smoke，但调用超时并进入不可中断 D 状态。M45 的 CRX9 协议探针成功连接且服务端接受 session，之后等待响应超时，服务端同样进入 D 状态。这些证据更支持 CoreX Driver/设备后端阻塞；XID 与后续卡死的时间相关性尚不能证明因果关系，也没有证据把问题归到 Remote 客户端或传输层。
- 2026-11-16 的 K3s smoke 事件只记录了 workload Pod 启动、被控制器标记 Ready 和释放 Lease，没有保留下来的应用退出码或数值校验结果，不能作为真实 GPU 执行通过的证据。与此同时，样例镜像存在可复现的配对错误：`driver_smoke_app` 查找 `driver_add`，但当时的 `corex-workload:arm64` 只包含 `session_lifecycle.cubin`，其中定义的是 `m1s2_lifecycle_kernel`。该错误会在模块函数查找阶段使 smoke 失败；它不能解释 native CoreX 调用进入 D 状态。

样例现已改为使用 `driver_kernel.cubin`，并新增可重复构建的 workload 镜像定义。CoreX 设备后端仍需在不触发共享 GPU 风险的条件下恢复后复测；在复测 native 与 K3s Remote 数值路径之前，不宣称 CoreX 真机数据面已通过。

### 23.4 当前 ARM 主机环境命令

以下命令按 root 执行。先为 `lvtong` 开通 Docker：

```bash
usermod -aG docker lvtong
getent group docker
```

用户必须重新登录，或在新的 `lvtong` shell 中执行：

```bash
newgrp docker
docker info
docker run --rm --platform linux/arm64 arm64v8/ubuntu:22.04 uname -m
```

当前仓库的 `go.mod` 要求 Go 1.26，因此不要使用 Ubuntu 22.04 默认的 Go 1.18。为避免修改系统 `/usr/local`，建议把官方 ARM64 工具链安装到 `lvtong` 的用户目录：

```bash
GO_VERSION=1.26.11
mkdir -p "$HOME/.cache"
curl -fL -o "$HOME/.cache/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz"
echo '6c2f29b458b5add0835f18b776f69c8881a6d88428f1484a6da55755e620757e  '"$HOME/.cache/go.tar.gz" | sha256sum -c -
mkdir -p "$HOME/.local/opt" "$HOME/.local/bin" "$HOME/.local/share/go"
rm -rf "$HOME/.local/opt/go-${GO_VERSION}"
tar -C "$HOME/.local/opt" -xzf "$HOME/.cache/go.tar.gz"
mv "$HOME/.local/opt/go" "$HOME/.local/opt/go-${GO_VERSION}"
printf '\nexport PATH="$HOME/.local/opt/go-%s/bin:$HOME/.local/bin:$PATH"\nexport GOPATH="$HOME/.local/share/go"\nexport GOBIN="$HOME/.local/bin"\n' "$GO_VERSION" >> "$HOME/.profile"
export PATH="$HOME/.local/opt/go-${GO_VERSION}/bin:$HOME/.local/bin:$PATH"
export GOPATH="$HOME/.local/share/go"
export GOBIN="$HOME/.local/bin"
go version
```

只有确实缺少系统构建工具时，才由 root 安装缺失的包；不要让 `lvtong` 直接运行 `apt-get`：

```bash
apt-get update
apt-get install -y build-essential git curl ca-certificates \
  cmake pkg-config python3 python3-pip jq rsync file binutils \
  unzip tar gzip
```

安装 controller-gen 和 envtest 工具到 `lvtong` 用户目录：

```bash
GOBIN="$HOME/.local/bin" go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0
GOBIN="$HOME/.local/bin" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
controller-gen --version
setup-envtest version
```

安装 ARM64 `kubectl`：

```bash
KUBECTL_VERSION="$(curl -fsSL https://dl.k8s.io/release/stable.txt)"
curl -fLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/arm64/kubectl"
curl -fLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/arm64/kubectl.sha256"
echo "$(cat kubectl.sha256)  kubectl" | sha256sum --check
install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl
rm -f kubectl kubectl.sha256
kubectl version --client
```

安装单节点 K3s（实验环境）：

```bash
curl -sfL https://get.k3s.io | sh -
cat >/usr/local/bin/kubectl <<'EOF'
#!/bin/sh
exec /usr/local/bin/k3s kubectl "$@"
EOF
chmod 0755 /usr/local/bin/kubectl
mkdir -p /home/lvtong/.kube
cp /etc/rancher/k3s/k3s.yaml /home/lvtong/.kube/config
chown -R lvtong:lvtong /home/lvtong/.kube
kubectl get nodes -o wide
```

K3s 自带 containerd；本机 Docker 主要用于构建镜像。构建出的镜像需要导入 K3s 的 containerd：

```bash
docker save mlu-remote-client:arm64 | k3s ctr images import -
docker save mlu-remote-server:arm64 | k3s ctr images import -
k3s crictl images | grep remote
```

不要默认假设 Docker daemon 的镜像缓存会被 K3s 使用；Docker 和 K3s containerd 是两个镜像存储空间。

如果 Docker 拉取 Docker Hub 超时，而 Go/curl 通过本地代理可以访问，原因通常是 Docker daemon 不继承当前 shell 的 `http_proxy`。不要立即重启已有 Docker 服务；先测试代理：

```bash
curl -I -x http://127.0.0.1:54321 https://registry-1.docker.io/v2/
```

如果返回 `401 Unauthorized`，说明代理链路正常。可以选择在维护窗口为 Docker daemon 配置可逆的 systemd drop-in：

```bash
sudo mkdir -p /etc/systemd/system/docker.service.d
sudo tee /etc/systemd/system/docker.service.d/http-proxy.conf >/dev/null <<'EOF'
[Service]
Environment="HTTP_PROXY=http://127.0.0.1:54321"
Environment="HTTPS_PROXY=http://127.0.0.1:54321"
Environment="NO_PROXY=localhost,127.0.0.1,::1"
EOF
sudo systemctl daemon-reload
sudo systemctl restart docker
docker pull ubuntu:22.04
```

撤销时删除该 drop-in、执行 `systemctl daemon-reload` 并重启 Docker。Docker 已运行其他容器时，重启前应先确认业务影响；更保守的方式是在可联网的另一台机器上 `docker save`，再把 tar 导入本机 Docker。
```
