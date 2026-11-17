# 合成 LocalOnly Adapter 容器验证

这个样例用于在**没有本地物理 GPU、没有 NVIDIA Device Plugin** 的 kind 集群中，验证平台的 Local Adapter 容器控制路径。它不执行 CUDA，也不能证明真实 GPU、MIG 或共享插件的隔离能力。

它临时在一个 kind Node 的 `status.capacity`/`status.allocatable` 发布一个唯一的扩展资源 `example.com/synthetic-gpu`，然后运行普通 Ubuntu 容器。脚本验证：

- `kubernetes-device-plugin` Provider 和 Local Pool 成为 `Ready`；
- `LocalOnly` Workload 选择 `Local`，并绑定到被标记的 Node；
- 平台向业务容器同时注入该扩展资源的 requests 和 limits；
- 不注入 `REMOTE_GPU_ADDRESS`、`CRICKET_RPCID`、`LD_PRELOAD` 或 Cricket init container；
- Pod 成功，`AcceleratorLease` 最终 `Released`；
- 结束时删除该 Workload、Provider、Pool、Node label 和临时资源，避免污染 kind Node。

## 前置条件

1. 当前 manager 已部署到目标集群；
2. `cricket-image-demos:cuda12.8` 已经能被目标 Node 获取（先运行复杂图像镜像构建步骤即可）；
3. 对 Node status 有 patch 权限；
4. 仅在测试集群执行，绝不能在真实生产 Node 上伪造 capacity。

kind 环境中若尚未加载复杂图像镜像，先构建并加载：

```bash
cd /mnt/d/vGPU/accelerator-control-plane
KIND_CLUSTER=cricket-e2e ./examples/cricket-e2e/complex-demo/build-complex-images.sh
```

运行：

```bash
./examples/local-synthetic-e2e/run.sh
```

若集群有多个 Node，可以明确指定：

```bash
SYNTHETIC_LOCAL_NODE=cricket-e2e-control-plane \
  ./examples/local-synthetic-e2e/run.sh
```

成功输出应包含：

```text
selectedMode: Local
injected local resources: ... example.com/synthetic-gpu ...
Cricket variables: REMOTE_GPU_ADDRESS=unset CRICKET_RPCID=unset LD_PRELOAD=unset
synthetic LocalOnly adapter container test completed
```

## 与真实 Local GPU E2E 的区别

该实验只证明本地平台适配层的选择、Lease、资源注入、Node 绑定和“不注入远端运行时”的契约。真实 Local GPU E2E 仍需部署 NVIDIA Device Plugin 或目标共享插件，使用其实际发布的资源名，并让业务 CUDA/NPU 程序在真实硬件上完成数值验证。
