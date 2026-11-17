# 本地无卡、远端有卡的真实验证

这组脚本把验证边界分开：本地机器不需要 GPU/NPU，也不把本地设备伪装成真实设备；本地只负责编译/构建镜像和提交 Kubernetes 对象。GPU/NPU 访问只发生在远端 server Pod。

## 本地无卡验证

先验证控制面和镜像可以构建：

```bash
go test ./...
docker build -t accelerator-control-plane:dev .
```

需要验证 Local 选择、Lease 和 Pod 注入但没有真实卡时，运行合成测试：

```bash
./examples/local-synthetic-e2e/run.sh
```

它只临时发布一个 `example.com/synthetic-gpu` 扩展资源，不执行 CUDA/MLU，结束时会清理 Node 状态。

## 远端真实 GPU/NPU 验证

先把对应的 workload/client/server 镜像构建并加载到集群，然后在控制面节点执行：

```bash
REMOTE_DRIVER=corex DEPLOY_SERVER=true ./examples/remote-real-smoke/run.sh
REMOTE_DRIVER=mlu   DEPLOY_SERVER=true ./examples/remote-real-smoke/run.sh
```

脚本会等待 `AcceleratorWorkload=Succeeded`，检查 worker 退出码为 0，并保存 Workload、Lease、Pod describe/events、client stdout/stderr 到 `docs/evidence/remote-real-smoke/<driver>/`。数值校验由镜像内真实 GPU/NPU 程序负责；脚本不会把“容器启动”当作计算成功。

如果 server 已经部署，省略 `DEPLOY_SERVER=true`。远端 server 清单中的设备挂载和 `privileged` 仅适用于单机实验，生产环境应替换为设备插件和最小权限配置。
