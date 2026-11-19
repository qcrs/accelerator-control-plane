# 真实 RemoteOnly smoke

使用新版 `arm64-observed-v1` workload/client/server 镜像。CoreX 执行 256 字节 Driver kernel 校验，MLU 执行 1024 元素 BANG vector-add 与 CPU reference 比对；旧内存复制 smoke 不算 kernel 通过。

```bash
REMOTE_DRIVER=corex ./examples/remote-real-smoke/run.sh
REMOTE_DRIVER=mlu ./examples/remote-real-smoke/run.sh
```

可用 `DEPLOY_SERVER=true` 部署样例服务端；这会修改集群。脚本会删除并重建它自己的 smoke Workload。样例 namespace 固定为 default。

每次生成独立 evidence 目录。通过必须同时满足：

1. Workload Succeeded，引用与 Pod/Lease 匹配；
2. Pod worker 正常结束且 exit code=0；
3. 日志含明确的 kernel 数值正确标记；
4. Lease Released；
5. Endpoint 最新远端观察可信，activeSessions=0、reserved=0、Ready=True。

Failed、超时、缺 Pod/Lease、日志 EOF、缺数值标记、清理不确定均返回非零。保存 Workload/Lease/Endpoint/Pod JSON、describe/events、client 日志、server 日志和采集错误。

不修改集群的单 ARM 实验、runtime 补丁、构建和镜像验收见 [闭环说明](../../docs/remote-observation-and-validation.zh-CN.md)。

原合成 LocalOnly 示例仅测试本地资源注入，不是“本地无卡后选择远端”的证明；本轮无卡证明来自隐藏 `/dev`/SDK 的真实 Remote 客户端和没有设备挂载的 workload 容器。

## 新版 K3s 生命周期验收

新增 `k3s_suite.py` 使用独立测试命名空间，覆盖 Auto 选择远端、明确数值日志、独占竞争、停服排队、Ready 后故障不重放、删除回收、Controller 重启和 CoreX DNS/Pod IP 变化。准备及执行步骤、当前实际结果见 [K3s 验收记录](../../docs/k3s-validation-report.zh-CN.md)。

脚本在创建任务前校验日志通道、镜像 config digest 和空闲状态；没有完整证据时返回 FAIL。`--disruptions` 会操作测试服务和控制器，并拒绝其他命名空间的活动任务或 Claim。上面的单项样例入口仍可使用。
