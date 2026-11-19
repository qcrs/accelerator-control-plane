# Cricket 独立远程 GPU 调用说明手册与实验记录

## 0. 文档定位

本文面向第一次接触本项目的开发者，说明如何在不使用 Kubernetes、不使用业务容器、也不依赖任何平台控制面的前提下，使用 Cricket 让一台计算机上的普通 CUDA 程序调用另一台计算机上的 NVIDIA GPU。

本文只讨论 Cricket 独立数据面：

```text
本地 CUDA 程序
  -> Cricket Client
  -> 局域网 RPC
  -> Cricket Server
  -> 远端 NVIDIA GPU
```

本文不讨论资源调度、Provider、Pool、Lease、Pod、镜像或平台自动选择。Cricket 独立模式没有“调度器”；它只把已经指定目标地址的 CUDA API 调用转发到远端。

文档同时承担三种用途：

1. 架构与原理说明；
2. 第一次部署和日常启动手册；
3. 实验步骤、结果记录和现场截图规划。

本文保留了 2026 年 8 月 20 日重新执行实验所得的数值结果和原始终端日志，但不把程序渲染的日志图片当作现场截图。文中的 C-01～C-08 是预留给真实 PowerShell、WSL 或 SSH 终端截图的插入位置；截图将在对应命令重新执行时逐张采集。

### 0.1 建议阅读顺序

第一次接触本项目时，建议按下面的顺序阅读，而不是直接复制命令：

1. 先读第 1～3 章，理解本文要证明的能力、Cricket 的调用原理以及两台机器各自负责什么；
2. 再读第 4～6 章，完成 Server 构建、远端启动和局域网连通性检查；
3. 然后依次执行三个 Demo：API 基础调用、CUDA Kernel、矩阵全连接层；
4. 最后使用第 10～13 章记录结果、排查错误并整理截图。

如果只想恢复已经部署好的环境，可以直接从第 5 章开始；如果要向他人解释“为什么这不是普通远程登录”，应重点阅读第 2.2、2.6 和 3.4 节。

### 0.3 现场截图规则

本文后续截图应遵守以下规则：

1. 截图必须来自实际 PowerShell、WSL、SSH 终端或 GPU 监控窗口，不能使用重新排版的日志图片冒充现场截图；
2. 尽量在一张图中保留完整命令、主机提示符、关键输出和退出码；
3. 调用端截图应能看出程序连接的是 `192.168.5.6:53773`，而不是本机 CUDA；
4. 远端截图应能看出远端主机身份、Cricket Server、RPC 注册或 GPU 活动；
5. 计算结果截图必须包含数值校验，不能只截 `TcpTestSucceeded=True`、`cudaSuccess` 或 GPU 型号；
6. 截图前可以清屏，但不能删除会改变结论的警告、错误或退出码；
7. 建议统一保存到 `docs/images/cricket-standalone/`，文件名使用 `C-01-...png` 到 `C-08-...png`；
8. 原始日志继续保存在 `docs/evidence/cricket-standalone/logs/`，作为截图之外的可复核记录。

每个截图位置都给出“截图目的”和“必须包含”。后续真正插图时，只需把占位块替换为对应 Markdown 图片引用，不需要改动实验结论。

### 0.2 术语表

本文第一次出现的英文术语按下表理解：

| 术语 | 中文解释 | 在本文中的含义 |
|---|---|---|
| Client | 客户端 | 位于调用端的 `cricket-client.so`，负责拦截并转发 CUDA 调用 |
| Server | 服务端 | 位于 GPU 端的 `cricket-rpc-server`，负责调用真实 CUDA/GPU |
| CUDA Runtime | CUDA 运行时接口 | `cudaMalloc`、`cudaMemcpy`、`cudaFree` 等以 `cuda*` 开头的常用 API |
| CUDA Driver API | CUDA 驱动接口 | `cuMemAlloc`、`cuModuleLoad` 等更底层的 `cu*` API |
| Kernel | GPU 核函数 | 由大量 GPU 线程并行执行的函数，例如 `add_one<<<...>>>()` |
| RPC | Remote Procedure Call，远程过程调用 | 把一次本地函数调用编码后交给另一台机器执行 |
| ONC RPC | 一种传统 RPC 协议族 | Cricket 当前提交使用的 RPC 实现，依赖 `rpcbind` 和 `libtirpc` |
| `rpcbind` | RPC 端口登记服务 | 保存 RPC program/version 与实际 TCP 端口的对应关系 |
| `LD_PRELOAD` | 动态库预加载机制 | 让 Linux 动态链接器优先加载 Cricket 的 CUDA 包装函数 |
| ELF | Linux 可执行与可链接文件格式 | 本文编译得到的程序和 `.so` 共享库格式 |
| ABI | Application Binary Interface，应用二进制接口 | 已编译程序与动态库在二进制层面的调用约定 |
| H2D | Host to Device，主机到设备 | 调用端内存数据传向远端 GPU 显存 |
| D2H | Device to Host，设备到主机 | 远端 GPU 结果返回调用端内存 |
| RTT | Round-Trip Time，网络往返时延 | 一个请求发送并收到响应所需的时间 |
| Smoke Test | 冒烟测试 | 用最小任务快速确认关键链路能否工作 |
| Artifact | 构建制品 | 编译产生的可执行文件或共享库，不是源代码 |
| Fat Binary | 多架构 CUDA 制品 | 同一程序中包含多个 GPU 架构代码或 PTX |
| PTX | CUDA 中间指令表示 | 可由兼容的远端驱动进一步即时编译成 GPU 机器码 |

## 1. 能力边界与当前结论

### 1.1 本文要证明什么

独立实验需要证明：

- 调用端能够加载 `cricket-client.so`；
- Client 能够连接远端固定入口；
- CUDA Runtime 调用能够转成 ONC RPC；
- 远端 Server 能识别 RPC program/version；
- 远端 GPU 能完成显存分配、kernel、同步和数据回传；
- GPU 输出与 CPU 参考结果一致；
- 远端 Windows/WSL 重启后能够一键恢复；
- 实验失败时能够区分网络、动态库、RPC 和 CUDA 错误。

### 1.2 本文不证明什么

本实验不证明：

- 自动寻找最合适的 GPU；
- 多租户和公平调度；
- Kubernetes 容器执行；
- 任意 CUDA API 都受支持；
- PyTorch、TensorFlow 等框架无需额外验证即可使用；
- MIG、fractional GPU、跨任务隔离或故障迁移；
- 远程执行性能等同于本地 PCIe GPU。

### 1.3 已有实验基线

| 项目 | 当前记录 |
|---|---|
| 远端系统 | Windows + Ubuntu 22.04.5 WSL2 |
| WSL 内核 | 6.18.33.2-microsoft-standard-WSL2 |
| GPU | NVIDIA GeForce RTX 5060 Ti 16 GB |
| CUDA Toolkit | 12.8，nvcc 12.8.93 |
| cuDNN | 9.12.0.46 |
| Cricket commit | `5d55bd582fc7acbb2a12c45f104020566a67218a` |
| libtirpc commit | `5ca4ca92f629d9d83e83544b9239abaaacf0a527` |
| RPC program/version | 99/1 |
| 远端 Windows LAN IP | `192.168.5.6` |
| 调用端 Windows LAN IP | `192.168.5.3` |
| 固定服务入口 | `192.168.5.6:53773` |

已有记录表明 API smoke、kernel smoke、D2H 回传和逐元素校验成功；但正式对外截图时，仍应按本文“跨机独立调用”步骤重新执行，不能用其他执行框架的日志代替。

## 2. Cricket 原理

### 2.1 总体结构

```text
┌──────────────────── 调用端 ────────────────────┐
│ CUDA 应用                                      │
│  cudaMalloc / cudaMemcpy / kernel / cudaFree  │
│                  │                             │
│                  v                             │
│ LD_PRELOAD=cricket-client.so                  │
│ CUDA 包装函数 -> 参数编码 -> ONC RPC Client   │
└──────────────────┬─────────────────────────────┘
                   │ TCP/IP LAN
                   │ RPC program=99, version=1
                   v
┌──────────────────── GPU 端 ────────────────────┐
│ Windows 固定入口 192.168.5.6:53773            │
│          │ portproxy                           │
│          v                                     │
│ WSL 动态 IP : Cricket 动态端口                 │
│ rpcbind + cricket-rpc-server                  │
│          │                                     │
│ CUDA Runtime / Driver / cuBLAS / cuDNN / NVML│
│          │                                     │
│ NVIDIA GeForce RTX 5060 Ti                    │
└────────────────────────────────────────────────┘
```

### 2.2 动态符号拦截

普通动态 CUDA 程序在启动时由动态链接器解析 `cudaMalloc`、`cudaMemcpy` 等符号。设置：

```bash
LD_PRELOAD=/path/to/cricket-client.so
```

后，动态链接器优先使用 Cricket 提供的包装函数。包装函数不会在调用端执行真实 CUDA，而是：

1. 读取 API 参数；
2. 将参数和必要数据编码为 RPC 请求；
3. 发送到 Cricket Server；
4. 等待远端返回 CUDA 错误码或数据；
5. 把结果转换成调用端程序预期的返回值。

因此业务源码不需要包含网络协议代码。决定调用路径的是运行时是否加载 Cricket Client，而不是程序在哪里编译。

### 2.3 动态 cudart 要求

测试程序必须动态链接 CUDA Runtime：

```bash
nvcc -cudart shared source.cu -o program
```

检查：

```bash
ldd ./program | grep libcudart
```

如果程序把 cudart 静态链接进 ELF，`LD_PRELOAD` 不一定能覆盖对应调用。特殊的 `dlopen`、私有封装、未覆盖 Driver API 或框架自定义加载路径也需要单独验证。

### 2.4 ONC RPC 与 rpcbind

固定版本使用：

```text
RPC program: 99
RPC version: 1
Transport: TCP
```

Server 启动参数：

```bash
cricket-rpc-server 1
```

Client 环境变量：

```text
CRICKET_RPCID=1
```

在该提交中，`CRICKET_RPCID` 实际表示 RPC version，两端必须一致。

只配置主机名时：

```text
REMOTE_GPU_ADDRESS=192.168.5.6
```

Client 会通过 TCP 111 查询 rpcbind。显式配置端口时：

```text
REMOTE_GPU_ADDRESS=192.168.5.6:53773
```

Client 直接连接固定入口。当前部署采用第二种方式，业务端不依赖动态 RPC 端口。

### 2.5 为什么内部端口会变化

Cricket Server 向 rpcbind 注册时可能获得新的内部端口。WSL NAT 地址也可能在重启后变化。因此部署分成两层：

```text
客户端永久使用：192.168.5.6:53773
Windows portproxy：自动转发
WSL 本次地址：172.29.x.x:<动态端口>
```

远端一键脚本会发现本次 WSL IP 和 Cricket 动态端口，再刷新固定入口。

### 2.6 一次 kernel 的数据流

以数组加一为例：

1. `cudaGetDeviceCount` 查询远端设备；
2. `cudaMalloc` 在远端 GPU 建立显存对象；
3. H2D `cudaMemcpy` 将数组通过 TCP 发送到远端；
4. Client 发送 kernel 模块、入口和参数；
5. 远端 GPU 执行 kernel；
6. `cudaDeviceSynchronize` 等待远端完成；
7. D2H `cudaMemcpy` 将结果传回调用端；
8. 调用端执行 CPU reference 校验；
9. `cudaFree` 释放远端资源。

Cricket 不是跨主机统一内存。传输量、API 次数和网络 RTT 会直接影响性能。

## 3. 两端环境与职责

### 3.1 调用端

调用端可以没有 NVIDIA GPU，但需要：

- Linux 或 WSL2 用户空间；
- 与目标架构兼容的 CUDA ELF；
- `cricket-client.so`；
- `libtirpc.so.3`；
- 可达 `192.168.5.6:53773` 的网络；
- 正确的 `LD_LIBRARY_PATH`；
- 结果校验和超时机制。

如果调用端要编译 `.cu`，还需要 CUDA Toolkit 和 `nvcc`。没有 GPU 时不能使用 `-arch=native`，必须明确目标架构或生成 fatbin。

### 3.2 GPU 端

GPU 端需要：

- 可工作的 Windows NVIDIA 驱动和 WSL GPU 映射；
- `/dev/dxg`；
- CUDA Toolkit 12.8；
- cuDNN 9.12；
- Cricket Server 和 libtirpc；
- `rpcbind`；
- 固定的 Windows LAN 地址；
- 仅允许可信调用端访问的防火墙规则。

### 3.3 版本记录原则

每次实验至少记录：

```text
日期和时区
两端 OS/WSL 版本
GPU 型号与 UUID
驱动和 CUDA 版本
cuDNN 版本
Cricket/libtirpc commit
编译命令
程序 SHA256
Client/Server SHA256
远端入口
退出码与数值校验
警告和异常
```

### 3.4 构建位置、程序位置和计算位置

这三个“位置”必须分开描述：

| 阶段 | 发生位置 | 作用 |
|---|---|---|
| 构建 | 可以在 GPU 端、调用端或独立构建机 | 把 `.cu` 源码编译成 Linux ELF 制品 |
| 程序运行 | 调用端 | 运行 CPU 业务逻辑，并通过 `LD_PRELOAD` 加载 Cricket Client |
| GPU 计算 | GPU 端 | Cricket Server 调用真实 CUDA，在 NVIDIA GPU 上执行 Kernel |

程序在哪里编译，不决定 GPU 最终在哪里执行。真正决定调用路径的是程序启动时是否加载 `cricket-client.so`，以及 `REMOTE_GPU_ADDRESS` 指向哪里。

当前独立实验采用下面的工程化折中：

```text
源码复制到 GPU 端
  -> 使用已安装的 CUDA 12.8 和目标 GPU 编译
  -> 只把 ELF 制品复制回调用端
  -> 调用端加载 Cricket Client 并启动该制品
  -> GPU 端 Cricket Server 执行 CUDA 请求
```

这样做是为了避免在调用端宿主 WSL 中再维护一套 CUDA Toolkit，并确保 Kernel 制品包含当前 RTX 5060 Ti 可执行的目标代码。它适合固定远端的部署验证，但不是 Cricket 的硬性要求，也不是多种异构 GPU 的最终构建方案。

如果源码和业务进程都允许长期放在 GPU 端，直接在 GPU 端编译并运行更简单，此时没有使用 Cricket 的必要。本文之所以把制品传回调用端，是为了验证另一种能力：**业务进程留在调用端，而 GPU 计算发生在另一台机器。**

### 3.5 两条 TCP 链路不能混为一谈

实验中可能出现两种网络传输：

```text
构建阶段：源码或 ELF 制品
  -> Git / SCP / HTTP / SMB 等普通文件传输

运行阶段：CUDA API 参数、H2D 数据、Kernel 信息和 D2H 结果
  -> Cricket RPC over TCP
```

把源码复制到 GPU 端不是 Cricket 的功能。此前通过 HTTP 下载脚本或制品时，底层虽然也使用 TCP，但那只是文件传输。只有调用端程序在 `LD_PRELOAD=cricket-client.so` 下运行后产生的 CUDA RPC，才属于 Cricket 远程调用链路。

### 3.6 多个异构 GPU 端时怎么办

不同 GPU 端可能具有不同的 GPU 架构、驱动、CUDA 和数学库版本。调用端编译并不要求两端文件系统完全相同，但制品必须满足目标端兼容条件。常见策略有：

1. 固定单个 GPU 端：在该端编译，传回制品，最容易复现；
2. 按兼容类别构建：为同一类 GPU/CUDA 环境生成一个制品，多台兼容机器复用；
3. 生成 Fat Binary：在一个 ELF 中包含多个目标 GPU 架构；
4. 保留 PTX：允许兼容驱动在目标端进行 JIT（Just-In-Time，即时编译）；
5. 无法建立兼容范围时：为不同目标分别构建并记录制品摘要。

本文只验证第一种固定环境，不宣称同一个制品已经覆盖任意 GPU、任意 CUDA 或任意 cuDNN 环境。多远端的能力登记、构建选择和调度属于另一类控制面问题，不在本文范围内。

## 4. 第一次构建 Cricket

### 4.1 安装依赖

Ubuntu 22.04：

```bash
sudo apt update
sudo apt install -y build-essential autoconf automake libtool pkg-config \
  rpcbind rpcsvc-proto libssl-dev libelf-dev \
  libvdpau-dev libegl1-mesa-dev
```

检查：

```bash
for cmd in gcc make rpcgen rpcbind nvcc; do
  printf '%-10s' "$cmd"
  command -v "$cmd" || exit 1
done
```

### 4.2 固定源码

```bash
mkdir -p ~/src
cd ~/src
git clone https://github.com/RWTH-ACS/cricket.git
cd cricket
git checkout --detach 5d55bd582fc7acbb2a12c45f104020566a67218a

git config submodule.submodules/libtirpc.url \
  https://github.com/alisw/libtirpc.git
git submodule sync --recursive
git submodule update --init --recursive

git rev-parse HEAD
git -C submodules/libtirpc rev-parse HEAD
```

只有默认 submodule 地址不可用时才替换 URL，不应随意升级固定 commit。

### 4.3 检查 CUDA 与 cuDNN

```bash
nvidia-smi
nvidia-smi -L
ls -l /dev/dxg
nvcc --version
readlink -f /usr/local/cuda
ldconfig -p | grep -E 'libcuda|libcudart|libcublas|libcusolver|libnvidia-ml'
ldconfig -p | grep libcudnn
```

只有 `nvidia-smi` 而没有 `nvcc`，表示驱动可用但 CUDA 开发 Toolkit 不完整。

### 4.4 编译

```bash
cd ~/src/cricket
make -C submodules
make -C cpu LOG=INFO
```

常见缺失：

| 错误 | 处理 |
|---|---|
| `vdpau/vdpau.h` | 安装 `libvdpau-dev` |
| `EGL/egl.h` | 安装 `libegl1-mesa-dev` |
| `cuda_runtime_api.h` | 检查 CUDA include |
| `cudnn.h` | 安装匹配 CUDA 的 cuDNN dev 包 |
| `gcc: no input files` | 检查自定义 flags 中是否意外换行 |

检查产物：

```bash
ls -lh cpu/cricket-rpc-server cpu/cricket-client.so
file cpu/cricket-rpc-server cpu/cricket-client.so
ldd cpu/cricket-rpc-server | grep 'not found' || true
ldd cpu/cricket-client.so | grep 'not found' || true
```

整理运行目录：

```bash
mkdir -p ~/src/cricket/bin
cp -a cpu/cricket-rpc-server cpu/cricket-client.so ~/src/cricket/bin/
cp -a submodules/libtirpc/install/lib/libtirpc.so* ~/src/cricket/bin/
ls -lh ~/src/cricket/bin
```

## 5. 远端日常启动

### 5.1 三个远端交付文件

远端 Windows 保存：

```text
C:\cricket-remote\start-cricket.ps1
C:\cricket-remote\start-cricket-server.sh
C:\cricket-remote\README.zh-CN.md
```

主入口是 `start-cricket.ps1`；Bash 文件由它自动调用；README 是远端操作说明。

### 5.2 一条命令恢复

远端管理员 PowerShell：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File C:\cricket-remote\start-cricket.ps1
```

成功输出示例：

```text
Cricket ready
GPU: GPU 0: NVIDIA GeForce RTX 5060 Ti (...)
WSL IP: 172.29.184.27
Cricket internal port: 51104
Fixed endpoint: 192.168.5.6:53773
Fixed SSH endpoint: 192.168.5.6:2222 -> WSL port 22
```

脚本自动启动 `sshd`、rpcbind 和 Cricket Server，读取 WSL IP 与 Cricket 动态端口，更新 portproxy、防火墙和两个固定入口：

```text
192.168.5.6:53773 -> 本次 WSL IP:Cricket 动态端口
192.168.5.6:2222  -> 本次 WSL IP:22
```

因此远端重启后只需重新运行这一条脚本，Cricket 与 SSH 转发都会自动匹配新的 WSL IP。

> **【现场截图位置 C-01：远端一键启动与固定入口恢复】**
>
> 截图目的：证明远端 Windows 重启后，一条启动命令能够恢复 WSL 服务、Cricket Server 和固定端口转发。
>
> 必须包含：管理员 PowerShell 提示符、启动命令、`Cricket ready`、GPU 型号、WSL IP、Cricket 内部动态端口、固定入口 `192.168.5.6:53773`、固定 SSH 入口 `192.168.5.6:2222` 和 `portproxy` 表。
>
> 建议文件名：`docs/images/cricket-standalone/C-01-remote-startup.png`
>
> 建议图注：**图 C-01　远端 Cricket 服务的一键恢复与固定入口映射。**

本轮原始记录中的 Cricket 内部动态端口为 `51104`，固定入口为 `192.168.5.6:53773`。动态端口可能在下次启动时变化，因此现场截图应以当次脚本输出为准。

### 5.3 远端检查

```powershell
Test-NetConnection 192.168.5.6 -Port 53773
Test-NetConnection 192.168.5.6 -Port 2222
netsh interface portproxy show v4tov4
wsl -d Ubuntu-22.04 -- rpcinfo -p 127.0.0.1
wsl -d Ubuntu-22.04 -- bash -lc `
  "pgrep -af 'rpcbind|cricket-rpc-server'"
```

调用端 WSL 还可以检查远端管理链路：

```bash
ssh cricket-remote 'hostname; whoami; nvidia-smi -L; nvcc --version'
```

> **【现场截图位置 C-02：远端进程与 RPC 注册】**
>
> 截图目的：证明 `rpcbind` 和 `cricket-rpc-server` 正在远端 WSL 中运行，而且 Cricket 已向 RPC 端口映射服务注册。
>
> 必须包含：远端主机提示符、`pgrep` 输出、`rpcinfo -p 127.0.0.1` 输出，以及 `program 99 / version 1 / tcp` 对应端口。
>
> 建议文件名：`docs/images/cricket-standalone/C-02-rpc-registration.png`
>
> 建议图注：**图 C-02　远端 Cricket 进程及 ONC RPC 注册状态。**

## 6. 局域网与调用端准备

### 6.1 网络拓扑

```text
路由器              192.168.5.1
调用端 Windows      192.168.5.3
GPU 端 Windows      192.168.5.6
GPU 端 WSL          动态 172.29.x.x
固定 Cricket 入口   192.168.5.6:53773
固定 WSL SSH 入口   192.168.5.6:2222
```

建议在路由器使用 DHCP 地址保留，关闭 AP/无线客户端隔离，不配置公网 WAN 端口映射。

### 6.2 调用端网络验收

Windows：

```powershell
Test-NetConnection 192.168.5.6 -Port 53773
```

本地 WSL：

```bash
timeout 5s bash -c '</dev/tcp/192.168.5.6/53773'
echo $?
```

> **【现场截图位置 C-03：跨机固定入口连通】**
>
> 截图目的：证明调用端通过局域网访问的是 GPU 端 Windows 固定入口。
>
> 必须包含：调用端 Windows PowerShell 提示符、`Test-NetConnection 192.168.5.6 -Port 53773`、`SourceAddress=192.168.5.3` 和 `TcpTestSucceeded=True`。如果版面允许，可在第二张子图中加入 `ssh cricket-remote 'hostname; whoami; nvidia-smi -L'`，证明 SSH 后面的主机确实是 GPU 端。
>
> 建议文件名：`docs/images/cricket-standalone/C-03-cross-machine-network.png`
>
> 建议图注：**图 C-03　调用端到远端 Cricket 固定入口的局域网连通性。**

TCP 成功只证明网络入口可达，不能替代后续 CUDA Kernel 与数值正确性实验。

TCP 成功只证明网络入口可达，不能替代 CUDA 数值实验。

### 6.3 调用端 Runtime

调用端准备：

```text
~/cricket-standalone/bin/cricket-client.so
~/cricket-standalone/bin/libtirpc.so.3
~/cricket-standalone/tests/<测试程序>
```

检查：

```bash
ldd ~/cricket-standalone/bin/cricket-client.so | grep 'not found' || true
```

### 6.4 为什么设计三个 Demo

三个 Demo 不是简单重复，而是逐级增加证据强度：

| Demo | 增加的能力 | 主要输入/输出 | 能证明什么 | 单独不能证明什么 |
|---|---|---|---|---|
| Demo 1：API Smoke | 设备查询、显存分配、填充和释放 | 4 KiB 显存对象、CUDA 返回码 | Client/Server、基础 CUDA Runtime API 和远端显存管理可工作 | 自定义 Kernel 能执行、数据结果正确 |
| Demo 2：Kernel Smoke | H2D、Kernel、同步、D2H、CPU 校验 | 256 个整数、逐元素结果 | 自定义 Kernel 确实执行，结果能跨网返回且数值正确 | 较大输入和多阶段任务的稳定性 |
| Demo 3：矩阵全连接层 | 两个 Kernel、约 1.34 亿次乘加、较大 H2D/D2H、CPU 全量复算 | 本地 `512×512` 特征矩阵、权重矩阵和远端返回矩阵 | 本地输入确实经网络上传，远端完成经典矩阵计算，全部 262,144 个输出正确返回 | 生产性能、任意深度学习框架兼容性 |

证据应按下面的强弱顺序理解：

```text
TCP 端口可达
  < rpcbind 中存在 program 99/version 1
  < CUDA API 返回成功
  < Kernel launch/synchronize 成功
  < D2H 数据返回且 CPU 校验为 0 个错误
  < 多阶段任务重复执行仍全部正确
```

因此，端口检查属于前置条件，CPU Reference（CPU 参考实现）才是计算正确性的核心证据。

## 7. Demo 1：API Smoke

### 7.1 目标

只验证设备发现、显存分配、显存填充和释放，不执行自定义 kernel。

核心程序：

```cpp
#include <cuda_runtime.h>
#include <stdio.h>

int main(void) {
    int count = -1;
    cudaError_t err = cudaGetDeviceCount(&count);
    printf("cudaGetDeviceCount: err=%d count=%d\n", (int)err, count);
    if (err != cudaSuccess || count < 1) return 1;

    void *ptr = NULL;
    err = cudaMalloc(&ptr, 4096);
    printf("cudaMalloc: err=%d ptr=%p\n", (int)err, ptr);
    if (err != cudaSuccess) return 2;

    err = cudaMemset(ptr, 0x5a, 4096);
    printf("cudaMemset: err=%d\n", (int)err);
    if (err != cudaSuccess) return 3;

    err = cudaFree(ptr);
    printf("cudaFree: err=%d\n", (int)err);
    return err == cudaSuccess ? 0 : 4;
}
```

编译：

```bash
nvcc -cudart shared cricket-smoke.cu -o cricket-smoke
```

当前实验在 GPU 端执行这条编译命令，再通过普通文件传输把 `cricket-smoke` ELF 制品复制到调用端。该步骤不属于 Cricket RPC；正式远程调用从调用端执行下一条命令时才开始。

远程执行：

```bash
timeout 30s env \
  REMOTE_GPU_ADDRESS=192.168.5.6:53773 \
  CRICKET_RPCID=1 \
  LD_PRELOAD="$HOME/cricket-standalone/bin/cricket-client.so" \
  LD_LIBRARY_PATH="$HOME/cricket-standalone/bin:/usr/local/cuda/lib64" \
  ./cricket-smoke
echo $?
```

通过条件：四个 API 返回 0、count 大于 0、程序退出码 0。

> **【现场截图位置 C-04：独立 API Smoke】**
>
> 截图目的：证明普通动态 CUDA 程序通过 `LD_PRELOAD` 进入 Cricket Client，并完成基础 CUDA Runtime API 远程调用。
>
> 必须包含：调用端 WSL 提示符、完整启动命令或至少关键环境变量、连接地址 `192.168.5.6:53773`、`cudaGetDeviceCount`、`cudaMalloc`、`cudaMemset`、`cudaFree`、`api-call-cnt: 4` 和 `exit=0`。
>
> 建议文件名：`docs/images/cricket-standalone/C-04-api-smoke.png`
>
> 建议图注：**图 C-04　Cricket 独立模式下的基础 CUDA Runtime API 调用。**

本轮原始日志结果为四个 CUDA Runtime API 全部返回 0、远端设备数为 1、`api-call-cnt=4`、退出码为 0。

注意：C-04 不要求远端 `cricket-server.log` 在屏幕上逐条滚动。当前 Server 以 INFO 级别构建，基础 `cudaGetDeviceCount`、`cudaMalloc`、`cudaMemset` 和 `cudaFree` 不一定分别生成可见的 Server 日志；同时该任务通常只持续几十毫秒。C-04 的验收依据是调用端明确连接固定远端地址，并收到四个 API 的成功返回和 `exit=0`。远端进程与 RPC 注册由 C-02 证明，远端 GPU 同期活动由 C-07 的重复复杂负载证明。

## 8. Demo 2：Kernel 与逐元素校验

### 8.1 任务设计

输入为 `0..255`，远端 GPU 执行 `data[i] += 1`，调用端取回结果并与 CPU 期望值比较。

它覆盖：

```text
设备发现 -> cudaMalloc -> H2D -> kernel -> synchronize -> D2H -> CPU 校验 -> free
```

### 8.2 完整核心代码

```cpp
#include <cuda_runtime.h>
#include <stdio.h>

__global__ void add_one(int *data) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i < 256) data[i] += 1;
}

int main(void) {
    int count = 0;
    cudaError_t err = cudaGetDeviceCount(&count);
    printf("device count: err=%d count=%d\n", (int)err, count);
    if (err != cudaSuccess || count < 1) return 1;

    int host[256];
    for (int i = 0; i < 256; ++i) host[i] = i;

    int *device = NULL;
    if (cudaMalloc(&device, sizeof(host)) != cudaSuccess) return 2;
    if (cudaMemcpy(device, host, sizeof(host),
                   cudaMemcpyHostToDevice) != cudaSuccess) return 3;

    add_one<<<1, 256>>>(device);
    err = cudaGetLastError();
    printf("kernel launch: err=%d\n", (int)err);
    if (err != cudaSuccess) return 4;

    err = cudaDeviceSynchronize();
    printf("kernel sync: err=%d\n", (int)err);
    if (err != cudaSuccess) return 5;

    if (cudaMemcpy(host, device, sizeof(host),
                   cudaMemcpyDeviceToHost) != cudaSuccess) return 6;
    if (cudaFree(device) != cudaSuccess) return 7;

    int failures = 0;
    for (int i = 0; i < 256; ++i) {
        if (host[i] != i + 1) ++failures;
    }
    printf("verification failures: %d\n", failures);
    printf("sample: %d %d %d\n", host[0], host[1], host[255]);
    return failures == 0 ? 0 : 8;
}
```

### 8.3 编译策略

当前实验在目标 GPU 端编译，使用：

```bash
nvcc -cudart shared -arch=native \
  cricket-kernel-smoke.cu -o cricket-kernel-smoke
```

在没有 GPU 的调用端或构建机编译时必须明确远端架构，或者生成覆盖目标的 fatbin。

两种方式的取舍是：

| 方式 | 优点 | 局限 | 本文采用情况 |
|---|---|---|---|
| GPU 端编译后复制 ELF | 目标环境明确，可使用 `-arch=native` | 多个异构远端时可能重复构建；需要传源码或构建上下文 | 当前固定 RTX 5060 Ti 实验采用 |
| 调用端/构建机编译 | 源码不进入 GPU 端，便于统一构建 | 需要 Toolkit，并且必须提前知道目标架构和兼容范围 | 本文说明但未作为当前基线 |

无论采用哪种方式，通过条件都不能只看“编译成功”。必须在调用端以远端地址启动，并完成 D2H 回传和 CPU 数值校验，才能证明跨机计算链路。

检查动态 cudart：

```bash
ldd ./cricket-kernel-smoke | grep libcudart
```

### 8.4 远程执行

```bash
timeout 30s env \
  REMOTE_GPU_ADDRESS=192.168.5.6:53773 \
  CRICKET_RPCID=1 \
  LD_PRELOAD="$HOME/cricket-standalone/bin/cricket-client.so" \
  LD_LIBRARY_PATH="$HOME/cricket-standalone/bin:/usr/local/cuda/lib64" \
  ./cricket-kernel-smoke
rc=$?
echo "exit=$rc"
exit "$rc"
```

通过条件：

```text
device count: err=0 count=1
kernel launch: err=0
kernel sync: err=0
verification failures: 0
sample: 1 2 256
exit=0
```

> **【现场截图位置 C-05：跨机 Kernel 与数值校验】**
>
> 截图目的：证明自定义 CUDA Kernel 在远端执行，结果通过 D2H 返回调用端，并通过 CPU 逐元素校验。
>
> 必须包含：调用端 WSL 提示符、连接目标 `192.168.5.6:53773`、`kernel launch: err=0`、`kernel sync: err=0`、`verification failures: 0`、`sample: 1 2 256`、传输计数和 `exit=0`。
>
> 建议文件名：`docs/images/cricket-standalone/C-05-kernel-verification.png`
>
> 建议图注：**图 C-05　远端 CUDA Kernel 执行、结果回传及 CPU 数值校验。**

这是本文最重要的正确性截图。只有连接目标、Kernel、同步、D2H 后的 CPU 校验和退出码出现在同一次运行中，才能构成完整证据。

## 9. Demo 3：矩阵全连接层与 CPU 全量校验

### 9.1 为什么选择矩阵任务

Demo 1 和 Demo 2 证明了基础 API 与小型 Kernel 路径，但输入和计算量都很小。Demo 3 使用经典矩阵乘法，并包装成一个容易解释的全连接层：

```text
Y = ReLU(X × W + bias)
```

其中：

- `X` 是调用端本地生成的 `512×512` 特征矩阵；
- `W` 是调用端本地生成的 `512×512` 权重矩阵；
- `bias` 是长度为 512 的偏置向量；
- `X × W` 在远端 GPU 上使用分块矩阵乘法 Kernel 计算；
- 第二个远端 Kernel 完成逐列加偏置和 ReLU；
- `Y` 经 D2H 返回调用端；
- 调用端 CPU 对全部 262,144 个输出重新计算并逐项比较。

ReLU（Rectified Linear Unit，修正线性单元）的规则是：

```text
ReLU(value) = max(0, value)
```

因此该 Demo 既是经典矩阵乘法，也对应机器学习中常见的全连接层数据流。

### 9.2 数据规模与可读输出

完整矩阵包含 262,144 个元素，不适合全部打印。程序只展示 `X`、`W` 和 `Y` 左上角的 `4×4` 小块，同时对完整结果做校验。

计算与传输规模：

```text
矩阵乘加次数：512 × 512 × 512 = 134,217,728
H2D：X + W + bias = 2,099,200 字节
D2H：Y = 1,048,576 字节
完整输出元素：262,144
```

调用端使用固定 seed 生成输入，并在上传前打印 `X` 和 `W` 的 checksum。输入生成发生在调用端进程中；远端收到的是 Cricket 传输的矩阵字节，而不是源文件或预先准备的答案。

### 9.3 GPU 与 CPU 的职责

```text
调用端 CPU：生成 X/W/bias、打印输入样例和 checksum
      ↓ H2D，经 Cricket RPC
远端 GPU：tiled_matmul -> add_bias_relu
      ↓ D2H，经 Cricket RPC
调用端 CPU：完整复算 Y、逐项比较、输出 PASS/FAIL
```

程序使用整数输入和整数累加，因此 CPU 与 GPU 的正确结果必须完全一致，不需要浮点误差容忍。通过条件为：

```text
GPU result checksum == CPU result checksum
mismatched elements: 0 / 262144
maximum absolute difference: 0
verification: PASS
exit=0
```

### 9.4 源码、构建与运行

源码和脚本位于：

```text
examples/cricket-standalone/matrix-dense/
  matrix_dense.cu
  build-remote.sh
  run-remote.sh
  README.zh-CN.md
```

构建脚本把 CUDA 源码传到 GPU 端临时目录，使用远端 CUDA 12.8 和 `-arch=native` 编译动态 cudart ELF，再把 ELF 复制回调用端。编译文件传输不是 Cricket 数据调用；正式 Cricket 调用从本地执行 ELF 时开始。

```bash
cd /mnt/d/vGPU/accelerator-control-plane
./examples/cricket-standalone/matrix-dense/build-remote.sh
```

运行：

```bash
cd /mnt/d/vGPU/accelerator-control-plane
./examples/cricket-standalone/matrix-dense/run-remote.sh
```

也可以直接使用与前两个 Demo 相同的 `LD_PRELOAD` 方式启动 `docs/evidence/cricket-standalone/bin/cricket-matrix-dense`。

> **【现场截图位置 C-06：矩阵全连接层与 CPU 全量校验】**
>
> 截图目的：证明本地生成的大矩阵经 H2D 上传，远端完成约 1.34 亿次乘加和 ReLU，完整结果返回本地并通过 CPU 全量校验。
>
> 必须包含：调用端 WSL 提示符、远端地址、公式 `Y = ReLU(X x W + bias)`、矩阵尺寸、输入 seed、`X/W/Y top-left 4x4`、H2D/D2H 字节、GPU/CPU checksum、`mismatched elements: 0 / 262144`、`maximum absolute difference: 0`、`verification: PASS` 和 `exit=0`。
>
> 建议文件名：`docs/images/cricket-standalone/C-06-matrix-dense.png`
>
> 建议图注：**图 C-06　本地矩阵输入、远端 GPU 全连接层计算与本地 CPU 全量校验。**

本轮正式记录中，远端端到端阶段为 `525.909 ms`，本地 CPU 参考计算为 `209.399 ms`；GPU 与 CPU checksum 均为 `7489e7bed731fdf0`，262,144 个输出全部一致，最大绝对差异为 0，退出码为 0。另一次预运行的远端阶段约为 `218.782 ms`，说明耗时会受网络、CPU 和 GPU 状态影响，因此不作为固定通过阈值。

> **【现场截图位置 C-07：矩阵负载期间的远端 GPU 观测】**
>
> 截图目的：在调用端连续执行矩阵任务时，从 GPU 端观察 `nvidia-smi dmon` 的同期活动，补充证明计算发生在远端 GPU。
>
> 必须包含：远端主机提示符或窗口标题、`nvidia-smi dmon` 采样，以及与调用端重复矩阵任务重叠的时间段。由于单次任务较短，应连续运行多次后截图。
>
> 建议文件名：`docs/images/cricket-standalone/C-07-remote-matrix-gpu.png`
>
> 建议图注：**图 C-07　连续矩阵负载期间的远端 GPU 活动。**

C-07 是远端活动的补充证据；计算正确性的最终依据仍是 C-06 中的完整 D2H 返回、CPU 全量复算和零差异结果。

本轮使用 20 次连续矩阵任务配合 15 秒 `nvidia-smi dmon` 采样，20 次全部 PASS。采样期间远端 GPU SM 活动最高为 1%，显存活动最高为 6%。矩阵 Kernel 单次持续时间较短，1 秒采样会低估瞬时利用率；该数据只用于证明任务时间段内远端 GPU 出现活动，不用于性能评测。

## 10. 实验记录

### 10.1 已记录结果

| 实验 | 结果 | 证据状态 |
|---|---|---|
| 远端 GPU/CUDA 检查 | RTX 5060 Ti、CUDA Toolkit 12.8 | 本轮原始日志已有；C-03 现场截图待补 |
| rpcbind 与 program 99/version 1 | 正常注册，内部端口 51104 | 本轮原始日志已有；C-01/C-02 现场截图待补 |
| 固定入口恢复 | `192.168.5.6:53773 -> 172.29.184.27:51104` | 本轮原始日志已有；C-01 现场截图待补 |
| 调用端到远端 TCP | Cricket 53773、SSH 2222 均可达 | 本轮原始日志已有；C-03 现场截图待补 |
| API smoke | 四个 API 返回 0，API 调用计数 4，退出码 0 | 本轮原始日志已有；C-04 现场截图待补 |
| Kernel smoke | launch/sync 返回 0，memcpy 2048 字节 | 本轮原始日志已有；C-05 现场截图待补 |
| Kernel 数值正确性 | failures=0，sample=1/2/256，退出码 0 | 本轮原始日志已有；C-05 现场截图待补 |
| 矩阵全连接层 | `512×512`，约 1.34 亿次乘加，GPU/CPU checksum 一致，262,144 个结果零差异 | 本轮已重新执行；C-06 现场截图待补 |
| 20 次连续稳定性 | passed=20，failed=0，overall=PASS | 本轮原始日志已有；C-08 现场截图待补 |

不能把“TCP 可达”写成“CUDA 远程调用成功”；也不能用其他执行路径的日志填充独立实验截图。

本表结果来自 2026 年 8 月 20 日的重新执行。调用端日志、远端状态、数值校验和稳定性汇总保存在 `docs/evidence/cricket-standalone/`。目前这些原始日志用于保存实验事实，正式现场截图仍按 C-01～C-08 的位置另行采集。

### 10.2 实验记录表

```text
实验编号：
实验日期：
调用端：
GPU 端：
远端入口：
程序名称：
程序 SHA256：
编译命令：
Cricket commit：
libtirpc commit：
Client SHA256：
Server SHA256：
输入规模：
返回码：
CPU reference：
verification failures：
耗时：
API/传输计数：
警告：
日志路径：
截图编号：
结论：
```

本轮正式记录：

```text
实验日期：2026-08-20（Asia/Shanghai）
调用端：DESKTOP-OUOLT02 / WSL，用户 qcrs
GPU 端：DESKTOP-SMJDB0V / WSL，用户 pc
远端入口：192.168.5.6:53773
远端内部端口：172.29.184.27:51104
GPU：NVIDIA GeForce RTX 5060 Ti
CUDA 编译环境：CUDA Toolkit 12.8，nvcc 12.8.93
编译位置：GPU 端 WSL；使用 -arch=native 和 shared cudart
Client SHA256：fdb868ee5b49c95f4a2cb59c50ca7e854e0d301205891226cb228419313626ce
API Smoke SHA256：565eaa0b809c1c4dd8620e36b813c75523da905d7b416c79ad3c7af0a5548739
Kernel Smoke SHA256：44c9736a0f77107541f3bb7322cd0a13f4836ab426d627f0f68f008ad6bb5342
Matrix Dense SHA256：1d54561a78dfc61aee4e19b2db889d68f16dabca245824f8f7aed88b9a666a21
API Smoke：PASS，api-call-cnt=4，exit=0
Kernel Smoke：PASS，verification failures=0，memcpy-cnt=2048，exit=0
矩阵全连接层：PASS，512×512，134217728 次乘加，mismatches=0/262144
矩阵 GPU/CPU checksum：7489e7bed731fdf0 / 7489e7bed731fdf0
矩阵传输：H2D=2099200 字节，D2H=1048576 字节，Cricket memcpy-cnt=3147776
矩阵报告 SHA256：017866e6a4f3b7aa9a3bf6ca232dac31b64c70d5ad8f94772ca29cd268387d7c
稳定性：20/20 PASS
兼容性警告：unknown format: 0x2；本轮为非阻塞警告
原始日志：docs/evidence/cricket-standalone/logs/
结论：独立 Cricket 跨机 CUDA API、Kernel、矩阵全连接层、数据回传和 CPU 全量校验通过
```

### 10.3 稳定性测试

```bash
for i in $(seq 1 20); do
  echo "=== run $i ==="
  timeout 30s env \
    REMOTE_GPU_ADDRESS=192.168.5.6:53773 \
    CRICKET_RPCID=1 \
    LD_PRELOAD="$HOME/cricket-standalone/bin/cricket-client.so" \
    LD_LIBRARY_PATH="$HOME/cricket-standalone/bin:/usr/local/cuda/lib64" \
    ./cricket-kernel-smoke || exit "$i"
done
```

通过条件：20 次退出码与校验均为 0，Server 不退出，固定入口不变化。

> **【现场截图位置 C-08：20 次稳定性汇总】**
>
> 截图目的：证明跨机 Kernel 调用不是偶然成功，而是能够连续重复执行。
>
> 必须包含：调用端 WSL 提示符、测试说明、`passed=20`、`failed=0` 和 `overall=PASS`。不建议截取 20 次完整输出，只显示汇总并保留逐次原始日志。
>
> 建议文件名：`docs/images/cricket-standalone/C-08-stability-summary.png`
>
> 建议图注：**图 C-08　20 次连续跨机 Kernel 调用稳定性结果。**

本轮原始日志结果为 `passed=20`、`failed=0`、`overall=PASS`。

## 11. 排错

### 11.1 推荐顺序

1. 远端 `nvidia-smi -L`；
2. 远端 `/dev/dxg`；
3. CUDA/cuDNN 动态库；
4. rpcbind 进程和 TCP 111；
5. program 99/version 1；
6. Cricket Server 动态端口；
7. Windows portproxy；
8. 固定端口 TCP；
9. Client 和程序 `ldd`；
10. 动态 cudart；
11. RPC version；
12. GPU 架构与 API 覆盖。

### 11.2 常见现象

| 现象 | 优先检查 |
|---|---|
| `connection refused` | Server、动态端口、portproxy |
| `rpcinfo` 卡住 | TCP 111、rpcbind、防火墙 |
| `library not found` | `LD_LIBRARY_PATH`、libtirpc、cudart |
| `cudaGetDeviceCount` 失败 | Client 是否加载、地址、version |
| kernel 加载失败 | 目标架构、fatbin、CUDA 兼容性 |
| 返回码 0 但结果错误 | H2D/D2H 大小、CPU reference |
| WSL 重启后失联 | 重新运行远端一键脚本 |

`unknown format: 0x2` 是当前 Cricket ELF 解析器面对新 CUDA 产物时观察到的警告。只有在 kernel、同步、D2H 和数值校验全部成功时，才可记录为非阻塞警告。

## 12. 性能与安全

远程时间包含：

```text
序列化 + TCP RTT + H2D + GPU 计算 + D2H + 反序列化
```

应分别报告正确性、端到端延迟、传输数据量和吞吐。小 kernel 容易被网络延迟主导。

Cricket 当前没有 TLS 和客户端身份认证：

- 不向公网暴露 TCP 111 或 53773；
- 防火墙仅允许明确的调用端 IP；
- 使用可信 LAN、隔离 VLAN 或受控 VPN；
- 不让互不信任的用户共享同一个 Server；
- 不把敏感数据用于未加密实验。

## 13. 截图清单与最终验收

| 编号 | 内容 | 状态 |
|---|---|---|
| C-01 | 远端一键启动、动态端口和固定入口映射 | 待采集现场截图 |
| C-02 | 远端 rpcbind、Cricket Server 和 program 99 注册 | 待采集现场截图 |
| C-03 | 调用端到远端固定入口的局域网连通性 | 待采集现场截图 |
| C-04 | 独立 API smoke | 待采集现场截图 |
| C-05 | 独立 Kernel、D2H 回传与数值校验 | 待采集现场截图 |
| C-06 | 独立矩阵全连接层、H2D/D2H 和 CPU 全量校验 | 待采集现场截图 |
| C-07 | 连续矩阵负载期间的远端 GPU 观测 | 待采集现场截图 |
| C-08 | 20 次稳定性汇总 | 待采集现场截图 |

最终验收：

- [ ] 两个源码 commit 固定；
- [x] Client/Server/程序动态库无缺失；
- [x] 程序动态链接 cudart；
- [x] program 99/version 1 注册；
- [x] 固定入口可达；
- [x] 跨机独立程序显示远端连接地址；
- [x] malloc/H2D/kernel/sync/D2H/free 成功；
- [x] `verification failures: 0`；
- [x] 程序退出码为 0；
- [x] 重启恢复后再次成功；
- [ ] C-01～C-08 已替换为真实终端现场截图；
- [ ] 截图只展示 Cricket 独立执行路径；
- [ ] 防火墙只允许可信来源。

## 14. 结论

Cricket 通过动态符号拦截和 ONC RPC，把动态 CUDA 应用的受支持调用转发到远端 GPU。独立实验的关键证据不是端口可达，也不是设备名称，而是跨机程序在加载 Cricket Client 后完成 kernel、数据回传、CPU reference 校验并以 0 退出。

本手册中的独立数据面实验应长期保留，作为远程调用能力的基线和故障隔离依据。
