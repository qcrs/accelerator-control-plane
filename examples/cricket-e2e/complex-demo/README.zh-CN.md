# 复杂图像处理平台演示

## 1. 这个演示证明什么

这个目录不是单独运行 CUDA 二进制的例子，而是使用 Accelerator Control Plane 提交容器任务：

    AcceleratorWorkload
      -> Adapter Registry
      -> Provider / Allocator
      -> AcceleratorLease
      -> Cricket RuntimePlan
      -> init container + LD_PRELOAD
      -> 图像处理容器
      -> 远端 GPU

任务容器本身不会写 REMOTE_GPU_ADDRESS，也不会手工加载 cricket-client.so。地址和运行时由平台注入。

## 2. 三个复杂任务

| Workload | GPU 管线 | 输出 |
| --- | --- | --- |
| complex-image-pipeline | RGBA 生成、灰度化、3x3 Gaussian、Sobel、256-bin histogram | PGM 边缘图、checksum、直方图总数 |
| complex-batch-resize | 4 张合成图、双线性缩放、RGBA 统计和 CPU reference | PPM 缩略图、每图 RGB mean |
| complex-image-segmentation | 灰度化、5x5 Gaussian、阈值分割、3x3 majority morphology | PGM mask、前景像素数和比例 |

每个任务都执行：

- cudaGetDeviceCount；
- 多个 CUDA kernel；
- Host-to-Device 和 Device-to-Host 拷贝；
- cudaDeviceSynchronize；
- GPU 结果与 CPU reference 比对；
- verification failures 输出。

## 3. 构建复杂任务镜像

WSL：

    cd /mnt/d/vGPU/accelerator-control-plane
    ./examples/cricket-e2e/complex-demo/build-complex-images.sh

kind：

    KIND_CLUSTER=cricket-e2e ./examples/cricket-e2e/complex-demo/build-complex-images.sh

Windows PowerShell：

    powershell -ExecutionPolicy Bypass -File .\examples\cricket-e2e\complex-demo\build-complex-images.ps1 -KindCluster cricket-e2e

默认镜像：

    cricket-image-demos:cuda12.8

正式集群将 COMPLEX_IMAGE 或 PowerShell 参数替换为镜像仓库地址，并使用不可变 tag 或 digest。

## 4. 运行完整平台演示

先完成远端 Cricket、控制面和基础 Provider/Pool/Endpoint 部署：

    ./examples/cricket-e2e/deploy.sh

然后串行运行三个 RemoteOnly 复杂任务：

    ./examples/cricket-e2e/complex-demo/run-complex-demos.sh

任务必须串行，因为当前 Cricket Endpoint 是 ExclusiveEndpoint，一个静态 server 不能同时被多个 Workload 占用。

每个任务会打印：

- Workload phase；
- selectedMode；
- 生成 Pod 标签；
- 业务容器日志；
- Lease Released。

预期日志包括：

    injected remote address: 192.168.5.6:53773
    device count: 1
    verification failures: 0

## 5. 平台适配层测试矩阵

运行：

    ./examples/cricket-e2e/complex-demo/run-platform-matrix.sh

矩阵脚本默认优先使用 PATH 中的 Go；如果 WSL 没有 Go 但有 Docker，会自动使用
`golang:1.26.5-bookworm` 容器运行同一组测试。正式交付应使用 `GO_TEST_MODE=host` 或
`GO_TEST_MODE=container` 明确指定方式。容器模式默认使用 `https://goproxy.cn`，也可通过
`GOPROXY` 覆盖。仅在没有模块下载网络、而且需要先验证 Kubernetes
平台路径时，才使用：

    GO_TEST_MODE=skip ./examples/cricket-e2e/complex-demo/run-platform-matrix.sh

`skip` 会在输出中明确标记 Go 单测未执行，不能作为交付验收结果。

它分四层执行：

### 5.1 Adapter/Provider/Allocator 单元测试

    go test ./... -count=1

覆盖：

- API 类型、Manager、Adapter Registry、Provider、Allocator、Injector、Inventory、Policy
  和 Controller 全部包；
- Local Provider 的资源形态和 RuntimePlan；
- Cricket Provider 的地址、RPC ID、版本和 runtime 注入；
- LocalAllocator 的预留语义；
- RemoteAllocator 的 Endpoint snapshot、独占 claim 和释放；
- Workload Controller 的 Local/Remote 生命周期；
- Auto 选择、启动前回退、失败目标排除和 Ready 边界。

### 5.2 Kubernetes API/CRD 验证

脚本对五份 Workload（3 个 RemoteOnly、1 个 Auto、1 个 LocalOnly）执行 server-side dry-run，检查：

- PodTemplate metadata labels/annotations schema；
- placement.mode；
- fallback；
- env；
- workload names 和容器字段。

### 5.3 RemoteOnly 平台容器

三个图像任务经由 AcceleratorWorkload 创建，不允许应用镜像自己声明受管 GPU 资源，也不允许自己设置 Cricket loader 变量。

这层验证的是：

- Controller 选择 cricket driver；
- RemoteAllocator 占用 Endpoint；
- Cricket Provider 注入 init container 和环境变量；
- Pod 成功运行；
- Lease 最终 Released。

### 5.4 Auto 平台容器

可选执行：

    RUN_AUTO=true ./examples/cricket-e2e/complex-demo/run-platform-matrix.sh

这个任务使用：

    mode: Auto
    order: [Local, Remote]
    fallback.enabled: true

如果集群存在可用 Local Pool，它应优先本地；如果本地没有候选而远端可用，它应选择 Remote。要测试真实 Local -> Remote S2 回退，需要同时部署本地 Device Plugin 资源，并注入一个可归因于本地启动的失败；控制器已有 fake-client 单元测试覆盖该故障状态机。

### 5.5 LocalOnly 平台容器

如果目标集群安装了 NVIDIA Device Plugin，并且 config/samples/local.yaml 中的 nvidia.com/gpu Pool 已经 Ready，可以执行：

    RUN_LOCAL=true ./examples/cricket-e2e/complex-demo/run-platform-matrix.sh

该任务必须输出：

    selectedMode: Local
    injected remote address: (unset)
    verification failures: 0

它验证的是 Local Provider 生成扩展资源 requests/limits、选中 Node 约束、Pod 由本地 Device Plugin 提供 GPU，并且没有注入 Cricket client。这个任务在当前没有本地 GPU 的 kind/WSL 演示环境中会保持 Pending，因此默认不自动执行。

### 5.6 无本地 GPU 时的 Local Adapter 容器契约

为了不把本地 Adapter 的容器路径完全留给单元测试，仓库还提供一个只限 kind 测试环境的合成验证：

    ./examples/local-synthetic-e2e/run.sh

它临时在一个 Node 发布 `example.com/synthetic-gpu`，运行普通容器，并验证 `selectedMode: Local`、Node 绑定、requests/limits 注入，以及没有 `REMOTE_GPU_ADDRESS`、`CRICKET_RPCID`、`LD_PRELOAD` 或 Cricket init container。脚本会删除所有自身创建的资源和 Node 标记。

它不使用真实 GPU，不能代替 `RUN_LOCAL=true` 的 Device Plugin / CUDA 硬件验收；详细约束见 [local-synthetic-e2e/README.zh-CN.md](../../local-synthetic-e2e/README.zh-CN.md)。

## 6. 单独数据面与平台面不能互相替代

仍然要保留单独的 Cricket Smoke Test：

- 直接在远端 GPU 上运行 CUDA 程序；
- 手工使用 LD_PRELOAD 运行相同程序；
- 观察 rpcbind、端口和 CUDA 返回码。

复杂平台任务则验证：

- 用户只提交统一 Workload；
- 平台自动选择和注入；
- 适配器和 Lease 生效；
- Pod 完成后资源释放。

只有两层都通过，才能区分“Cricket 数据面坏了”和“控制面适配/注入坏了”。

## 7. 当前限制

- 复杂任务使用 CUDA 12.8、sm_120 和动态 CUDA runtime；
- 当前 Cricket 适配器支持整卡独占，不支持 MIG 或共享；
- 输出图片写在容器的 /tmp，任务日志打印摘要；需要保留图片时应增加 PVC 或对象存储输出；
- 复杂任务默认 RemoteOnly，避免本地无 GPU 的演示环境误选本地；
- Auto 只有在目标集群同时存在 Local 和 Remote 候选时才有意义；
- 合成 LocalOnly 样例只证明控制平面和 Pod 注入契约，绝不能作为物理 GPU、MIG 或共享插件隔离的验收；
- Ready 之后不会自动把应用迁移到另一种执行模式。
