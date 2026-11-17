# Cricket + AcceleratorWorkload 端到端示例

这套示例把已经手工验证成功的远端 RTX 5060 Ti 注册为：

```text
AcceleratorProvider -> AcceleratorPool -> AcceleratorEndpoint
```

然后提交一个 `AcceleratorWorkload`。控制器创建 Pod、独占 Lease，并自动注入 Cricket
客户端，使 Pod 中的 CUDA 测试程序连接 `192.168.5.6:53773`。

## 前提

- 远端 Windows/WSL 中的 `cricket-rpc-server 1` 正在运行。
- Windows 固定入口 `192.168.5.6:53773` 可从 Kubernetes 节点访问。
- 本机安装 Docker、kubectl 和一个 Kubernetes 集群。当前 WSL 验证环境可使用
  `docker.io + kind v0.32.0 + Kubernetes v1.36.1`。
- Cricket 客户端产物位于本机 WSL 的 `/home/qcrs/cricket-client`。

## 构建镜像

当前 WSL 环境：

```bash
cd /mnt/d/vGPU/accelerator-control-plane
./examples/cricket-e2e/create-kind-cluster.sh
KIND_CLUSTER=cricket-e2e ./examples/cricket-e2e/build-images.sh
```

Docker Desktop/Windows PowerShell：

```powershell
cd D:\vGPU\accelerator-control-plane
powershell -ExecutionPolicy Bypass -File .\examples\cricket-e2e\build-images.ps1
```

如果使用名为 `cricket-e2e` 的 kind 集群：

```powershell
powershell -ExecutionPolicy Bypass -File .\examples\cricket-e2e\build-images.ps1 -KindCluster cricket-e2e
```

生成三个镜像：

```text
accelerator-control-plane:dev
cricket-client:5d55bd5
cricket-remote-kernel-test:cuda12.8
```

## 部署

WSL：

```bash
./examples/cricket-e2e/deploy.sh
```

```powershell
powershell -ExecutionPolicy Bypass -File .\examples\cricket-e2e\deploy.ps1
```

两个部署脚本都可重复执行：它们只会删除并重建自身的 `remote-kernel-test` 样例 Workload，随后等待该 Workload 成功及其 Lease 释放。这样避免已分配 Workload 的不可变 spec 阻止后续演示部署。

## 查看远程计算结果

WSL：

```bash
./examples/cricket-e2e/observe.sh
```

```powershell
powershell -ExecutionPolicy Bypass -File .\examples\cricket-e2e\observe.ps1
```

成功日志必须包含：

```text
demo marker: remote-cricket-demo
injected remote address: 192.168.5.6
device count: err=0 count=1
kernel launch: err=0
kernel sync: err=0
verification failures: 0
sample: 1 2 256
```

## 可重复演示任务

完成部署后，可以提交一个带有 Pod 标签、注解和用户环境变量的演示任务：

```bash
./examples/cricket-e2e/run-demo.sh
```

这个任务会打印 Workload/Lease/Pod 状态、最终 Pod 标签，以及远端 CUDA 校验日志。
其中 `injected remote address` 由控制器注入，表示任务没有自己配置远端地址。

## 复杂图像处理与平台适配层矩阵

如果要验证的不只是单独的 Cricket CUDA 二进制，而是“用户提交容器 -> 平台适配层选择
Provider/Allocator -> 注入运行时 -> 容器访问 GPU”的完整路径，使用：

```bash
./examples/cricket-e2e/complex-demo/build-complex-images.sh
./examples/cricket-e2e/complex-demo/run-platform-matrix.sh
```

复杂镜像包含三个 CUDA 任务：多阶段 RGBA/Sobel 图像管线、批量双线性缩放，以及带形态学
后处理的图像分割。三个任务都通过 `AcceleratorWorkload` 的 `RemoteOnly` 路径运行，并检查
GPU 结果与 CPU reference，避免只看“进程启动成功”。

矩阵脚本还会运行适配器、Provider、Allocator 和 Controller 测试，以及 CRD server-side
dry-run。需要验证自动选择时执行：

```bash
RUN_AUTO=true ./examples/cricket-e2e/complex-demo/run-platform-matrix.sh
```

需要真实本地 Device Plugin 和本地 GPU 时执行：

```bash
RUN_LOCAL=true ./examples/cricket-e2e/complex-demo/run-platform-matrix.sh
```

`LocalOnly` 在没有本地 GPU 的 kind/WSL 环境中会保持 Pending，因此默认不执行；`Auto` 只有
在本地和远端候选都已注册时才有意义。详细任务、预期日志和限制见
[`complex-demo/README.zh-CN.md`](complex-demo/README.zh-CN.md)。

若当前 kind 没有本地 GPU，但需要验证 Local Adapter 是否正确选择 Node、注入扩展资源且不注入 Cricket，可运行仅限测试集群的合成容器验证：

```bash
./examples/local-synthetic-e2e/run.sh
```

它会暂时修改一个 Node 的测试 capacity，并在结束时回收；它不替代真实 Device Plugin/GPU 验收。详见 [`../local-synthetic-e2e/README.zh-CN.md`](../local-synthetic-e2e/README.zh-CN.md)。

## 清理工作负载

```powershell
kubectl delete -f .\config\samples\remote-cricket-5060ti.yaml
```

## 远端重启与文件传输

远端 Windows/WSL 每次重启后的 Cricket 一键恢复、三个远端文件的职责和故障检查见：

- [`remote/README.zh-CN.md`](remote/README.zh-CN.md)

从本地电脑临时传输这三个文件到远端的方法见：

- [`remote/TRANSFER.zh-CN.md`](remote/TRANSFER.zh-CN.md)
