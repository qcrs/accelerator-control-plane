# Cricket 接入无感切换平台使用说明

## 1. 文档定位

本文面向平台运维人员、应用开发人员和现场验收人员，说明把 Cricket 接入 Accelerator Control Plane 后，如何注册远端 GPU、提交任务、观察运行结果，以及如何使用 Local/Remote 自动选择和启动前回退。

本文是操作说明，不展开控制器内部实现。实现原理见 local-platform-layer-seamless-switching-design.zh-CN.md；网络准备见 network-two-lan-configuration.zh-CN.md；Cricket 运行验收见 cricket-runtime-call-and-validation-report.zh-CN.md。

## 2. 运行角色

部署完成后，各角色关系如下：

    本地 Kubernetes
      -> Accelerator Control Plane
      -> AcceleratorWorkload / AcceleratorLease / Pod
      -> Cricket client
      -> 192.168.5.6:53773
      -> 远端 Windows/WSL
      -> cricket-rpc-server
      -> RTX 5060 Ti

| 角色 | 必须做的事 |
| --- | --- |
| 远端 GPU 主机 | 驱动、CUDA、rpcbind、Cricket server、固定 LAN 入口 |
| Kubernetes 集群 | CRD、Controller、Provider/Pool/Endpoint、业务 Pod |
| 镜像仓库 | 正式环境保存 Controller、Cricket client 和业务镜像 |
| 用户 Workload | 只声明 GPU 请求、放置策略和业务 Pod template |

用户不需要在业务 YAML 中配置 LD_PRELOAD 或远端 IP。远端地址由 Endpoint 注册，控制器在绑定后注入。

## 3. 前置条件

### 3.1 远端主机

远端 Windows/WSL 或 Linux 主机必须满足：

    nvidia-smi
    rpcinfo -p 127.0.0.1
    ss -lntp | grep -E '(:111|:53773)'

远端 Cricket server 示例：

    CUDA_VISIBLE_DEVICES=0 /opt/cricket/bin/cricket-rpc-server 1

RPC ID 1 必须与 Endpoint 中的 parameters.rpcID 和 runtime.apiVersion 一致。

### 3.2 本地节点

本地开发/验收环境需要：

- Docker；
- kubectl；
- kind 或一个可用 Kubernetes 集群；
- 能访问远端 Endpoint；
- 能拉取或加载 Controller、Cricket client 和业务镜像。

kind 演示不要求本地节点有 GPU。RemoteOnly 任务的实际 CUDA 调用由远端 GPU 执行。

### 3.3 网络

确认：

    Test-NetConnection 192.168.5.6 -Port 111
    Test-NetConnection 192.168.5.6 -Port 53773

如果远端是 WSL2，必须先配置 Windows portproxy。不要把远端 WSL 的 172.29.x.x 地址填入 Endpoint。详细步骤见网络配置报告。

## 4. 准备 Cricket 客户端镜像

Cricket client 镜像至少包含：

    /opt/cricket/bin/cricket-client.so
    /opt/cricket/bin/libtirpc.so
    /opt/cricket/bin/libtirpc.so.3

当前 E2E 构建脚本默认从本地 WSL 的 /home/qcrs/cricket-client 取产物：

    cd /mnt/d/vGPU/accelerator-control-plane
    KIND_CLUSTER=cricket-e2e ./examples/cricket-e2e/build-images.sh

当前 kind 演示会生成：

    accelerator-control-plane:dev
    cricket-client:5d55bd5
    cricket-remote-kernel-test:cuda12.8

正式集群应改成镜像仓库地址，例如：

    registry.example.com/vgpu/accelerator-control-plane:0.1.0
    registry.example.com/vgpu/cricket-client:5d55bd5
    registry.example.com/vgpu/remote-kernel-test:cuda12.8

生产环境应使用不可变 tag 或 digest，并为私有仓库配置 imagePullSecrets。

## 5. 部署控制面

### 5.1 kind 演示

    cd /mnt/d/vGPU/accelerator-control-plane
    ./examples/cricket-e2e/create-kind-cluster.sh
    KIND_CLUSTER=cricket-e2e ./examples/cricket-e2e/build-images.sh
    ./examples/cricket-e2e/deploy.sh

部署脚本会：

1. 检查远端 Cricket TCP 端口；
2. 应用 config/default；
3. 等待 accelerator-manager Deployment Ready；
4. 应用远端 Provider、Pool、Endpoint 和示例 Workload。

### 5.2 通用 Kubernetes 集群

先构建并推送 Controller 镜像，再将 config/default 中的镜像替换为仓库地址：

    kubectl apply --server-side --force-conflicts -k config/default
    kubectl -n accelerator-system rollout status deployment/accelerator-manager --timeout=180s

检查 Controller：

    kubectl -n accelerator-system get pods
    kubectl -n accelerator-system get deployment accelerator-manager
    kubectl get crd acceleratorworkloads.accelerator.qcrs.io

## 6. 注册远端 Cricket 资源

当前示例文件是 config/samples/remote-cricket-5060ti.yaml，包含四个对象：

1. AcceleratorProvider：声明 driver=cricket 和 clientRuntime；
2. AcceleratorPool：声明 Remote、GPU、ExclusiveEndpoint；
3. AcceleratorEndpoint：声明远端地址、版本、RPC ID 和设备；
4. AcceleratorWorkload：提交一个 RemoteOnly CUDA 测试任务。

应用资源：

    kubectl apply -f config/samples/remote-cricket-5060ti.yaml

关键 Provider 字段：

    spec:
      driver: cricket
      deviceKinds:
        - GPU
      clientRuntime:
        image: cricket-client:5d55bd5
        runtimeVersion: 5d55bd582fc7acbb2a12c45f104020566a67218a
        sourcePath: /opt/cricket/bin
        mountPath: /opt/accelerator/cricket

关键 Pool 字段：

    spec:
      location: Remote
      deviceType: GPU
      remote:
        allocationMode: ExclusiveEndpoint

关键 Endpoint 字段：

    spec:
      address: 192.168.5.6:53773
      runtime:
        driver: cricket
        version: 5d55bd582fc7acbb2a12c45f104020566a67218a
        apiVersion: "1"
      parameters:
        rpcID: "1"

Endpoint.devices 必须与 Cricket server 的 CUDA_VISIBLE_DEVICES 可见设备一致。当前静态适配器要求 Workload count 等于 Endpoint 健康设备数量。

检查配置状态：

    kubectl get acceleratorprovider,acceleratorpool,acceleratorendpoint
    kubectl describe acceleratorprovider remote-cricket-5060ti
    kubectl describe acceleratorpool remote-cricket-5060ti
    kubectl describe acceleratorendpoint windows-5060ti

Provider/Pool 应有 Ready=True，Endpoint 至少应有 Configured=True。没有网络探针时，Configured 不等同于实时网络 Ready。

## 7. 提交 RemoteOnly 任务

最小任务：

    apiVersion: accelerator.qcrs.io/v1alpha1
    kind: AcceleratorWorkload
    metadata:
      name: remote-cricket-demo
      namespace: default
    spec:
      accelerator:
        deviceType: GPU
        vendor: nvidia
        count: 1
        containerName: worker
      placement:
        mode: RemoteOnly
        reservationTTL: 5m
      podTemplate:
        spec:
          restartPolicy: Never
          containers:
            - name: worker
              image: cricket-remote-kernel-test:cuda12.8

当前可重复演示直接执行：

    ./examples/cricket-e2e/run-demo.sh

它会删除并重建 default/remote-cricket-demo，避免使用上一次演示的终态对象。

RemoteOnly 的含义是：如果远端不可用，不转本地，任务保持 Pending 或 Failed，并在状态中记录原因。

## 8. 观察任务是否真的使用远端 GPU

查看 Workload、Lease 和 Pod：

    kubectl -n default get acceleratorworkload,acceleratorlease,pod -o wide

查看 Workload 状态：

    kubectl -n default get acceleratorworkload remote-cricket-demo -o yaml

查看生成 Pod：

    pod_name=$(kubectl -n default get acceleratorworkload remote-cricket-demo -o jsonpath='{.status.podRef.name}')
    kubectl -n default get pod "$pod_name" --show-labels
    kubectl -n default describe pod "$pod_name"

查看业务日志：

    kubectl -n default logs "$pod_name" -c worker

通过标准：

    demo marker: remote-cricket-demo
    injected remote address: 192.168.5.6:53773
    device count: err=0 count=1
    kernel launch: err=0
    kernel sync: err=0
    verification failures: 0
    sample: 1 2 256

同时应看到：

    Workload phase: Succeeded
    selectedMode: Remote
    Lease phase: Released

如果日志中没有 injected remote address，说明该 Pod 不是由当前 Cricket RuntimePlan 注入，应该检查 Provider、Pool、Endpoint 和 Workload 的 driver/引用关系。

## 9. 使用 Auto 做本地优先和启动前回退

Auto 需要同时存在一个 Local Pool 和一个 Remote Pool。Local Pool 由 Kubernetes Device Plugin 发布资源，例如 nvidia.com/gpu：

    placement:
      mode: Auto
      order: [Local, Remote]
      reservationTTL: 5m
      fallback:
        enabled: true
        maxAttempts: 2
        startupTimeout: 5m
        backoff: 5s

行为如下：

1. 本地有可用容量时，先选择 Local；
2. 本地不可调度或本地 runtime 在 Pod Ready 前失败时，记录失败历史；
3. 删除旧 Pod，等待 NotFound，释放本地 Lease；
4. 重新选择 Remote，并注入 Cricket runtime；
5. 远端 Pod 第一次 Ready 后，不再执行自动跨模式回退。

Auto 不是把一个正在运行的进程从本地搬到远端。它是 Pod 启动阶段的有界重新选择，应用应避免在未 Ready 前产生不可重复的外部副作用。

启用 Auto 前先应用本地资源示例：

    kubectl apply -f config/samples/local.yaml
    kubectl apply -f config/samples/remote-cricket-5060ti.yaml

然后提交自己的 Workload。必须确保目标集群真的存在对应的 Device Plugin 和本地扩展资源，否则本地候选不会出现，任务会直接选择远端或等待资源。

## 10. 手工选择模式

只验证本地：

    placement:
      mode: LocalOnly

只验证远端：

    placement:
      mode: RemoteOnly

本地优先并允许一次远端回退：

    placement:
      mode: Auto
      order: [Local, Remote]
      fallback:
        enabled: true
        maxAttempts: 2

Workload 开始分配后，spec 不能随意修改。需要改变镜像、资源请求或放置策略时，删除终态对象并重新提交一个新的 Workload。

## 11. Endpoint 变更与服务重启

Endpoint 的地址、runtime 和 parameters 会复制到 Lease binding snapshot。修改 Endpoint 只影响后续新 Lease：

    kubectl patch acceleratorendpoint windows-5060ti --type merge -p '{"spec":{"address":"192.168.5.6:53774"}}'

修改后必须同步：

1. Windows portproxy；
2. Windows 防火墙；
3. Endpoint.spec.address；
4. 远端 rpcbind/Cricket 服务；
5. 新任务的网络验收。

已经绑定的 Workload 不会被自动改址。服务重启后应先确认 rpcinfo -p 和 TCP 连通，再提交新任务。

## 12. 常见故障

| 现象 | 优先检查 |
| --- | --- |
| Endpoint Configured=False | driver、地址、runtime.version、rpcID、设备 ID |
| Pod 卡在 Init | cricket-client 镜像、sourcePath、/bin/cp -a、镜像拉取 |
| Pod 没有远端环境变量 | Lease driver、Provider 引用、RuntimePlan、Pod 注入冲突 |
| connection timeout | TCP 111、RPC 端口、WSL portproxy、Windows 防火墙、无线隔离 |
| device count 为 0 | 远端 nvidia-smi、CUDA_VISIBLE_DEVICES、CUDA ABI |
| Workload 一直 Pending | Pool selector、Endpoint 健康、资源数量、Claim 冲突 |
| 回退没有发生 | mode 不是 Auto、Pod 已经 Ready、失败原因不是平台可回退类别 |
| 任务重复执行 | 检查 Workload 是否启用 fallback，以及应用是否在 Ready 前产生外部副作用 |

查看失败历史：

    kubectl -n default get acceleratorworkload remote-cricket-demo -o jsonpath='{.status.fallback.history}'

查看事件：

    kubectl -n default get events --sort-by=.lastTimestamp

## 13. 清理演示环境

删除示例资源：

    kubectl delete -f config/samples/remote-cricket-5060ti.yaml
    kubectl -n default delete acceleratorworkload remote-cricket-demo --ignore-not-found

删除 kind 集群：

    kind delete cluster --name cricket-e2e

正式环境不要直接删除 Provider/Pool/Endpoint。应先停止新 Workload，等待现有 Lease Released，再按变更流程下线 Endpoint 和远端服务。

## 14. 交付前检查

- [ ] Controller 镜像来自可访问的镜像仓库，且使用不可变版本。
- [ ] Cricket client 镜像包含正确 commit 的动态库。
- [ ] 业务镜像使用动态 CUDA runtime。
- [ ] Provider clientRuntime.runtimeVersion 与 Endpoint runtime.version 一致。
- [ ] Endpoint 地址是稳定 LAN/VPN 地址，不是 WSL NAT 地址。
- [ ] rpcbind 和 Cricket RPC 端口可达。
- [ ] 远端服务有自动启动和重启方案。
- [ ] 防火墙只允许控制面节点访问。
- [ ] 已确认当前只支持整卡独占，不把它描述成 MIG 或共享 GPU。
- [ ] 已记录 PyTorch/训练框架的单独兼容性结论。
- [ ] 已准备 Endpoint 重启、网络中断和镜像拉取失败的故障演练。
