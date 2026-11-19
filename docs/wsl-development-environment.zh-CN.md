# WSL 开发与验证环境

> 适用仓库：`accelerator-control-plane`。
>
> 本文只说明控制面项目在开发机 WSL 中的 Go、Docker、kind、kubectl 与 envtest 工具链；不改变 Cricket 远端 GPU 的部署方式，也不替代远端运行手册。
>
> 最近核验：2026-07-31，Windows 主机上的 WSL2 `Ubuntu`，仓库位于 `/mnt/d/vGPU/accelerator-control-plane`。

## 1. 目标与边界

项目的控制器、单元测试、CRD envtest 和 kind 演示应在 WSL Linux 环境中执行。Windows PowerShell 负责启动 WSL 或查看 Windows 网络配置；不要在 Windows 宿主机上混用另一套 Go 或 Docker CLI 来构建本项目。

当前已经确认可用的基线如下：

| 组件 | 已核验版本/状态 | 用途 |
|---|---|---|
| WSL 发行版 | `Ubuntu`（WSL2） | 控制面开发环境 |
| Go | `go1.26.5 linux/amd64` | 与 `go.mod` 的 `go 1.26.0` 匹配 |
| Docker Engine | Client/Server `29.1.3` | 构建镜像、运行 kind |
| kind | `v0.32.0` | 本地 Kubernetes 演示集群 |
| kubectl | Client `v1.36.2` | 集群部署、观察与验收 |
| controller-gen | `v0.21.0`，位于 `/usr/local/bin` | 与现有 CRD 生成标记一致的 DeepCopy/CRD 生成器 |
| setup-envtest | 已安装于 `~/.local/bin/setup-envtest` | 获取 API server/etcd 测试资产 |
| kind 集群 | `cricket-e2e`，控制面节点 `Ready` | 当前本地 E2E 目标 |

Go 安装在 `/usr/local/go`，`go` 和 `gofmt` 通过 `/usr/local/bin` 暴露，因此新的 WSL shell 也可直接使用。Docker socket 为 `root:docker`、权限 `0660`，开发用户 `qcrs` 已在 `docker` 组。

## 2. 进入正确的工作目录

在 Windows PowerShell 中进入 WSL：

```powershell
wsl.exe -d Ubuntu
```

随后在 WSL 中执行：

```bash
cd /mnt/d/vGPU/accelerator-control-plane
pwd
git status --short
```

`/mnt/d/...` 是 Windows 工作区的挂载路径，不是另一份 clone。所有源码、CRD、文档和样例都应只维护这一份；不要为了编译另外复制一份仓库，否则很容易让镜像、清单和实际源码脱节。

## 3. Go 工具链与模块下载

项目在 `go.mod` 中声明 Go 1.26。使用系统 apt 提供的旧版 Go（例如 Ubuntu 22.04 默认的 Go 1.22）不能作为本项目的交付基线。

核验命令：

```bash
go version
go env GOROOT GOPATH GOPROXY
```

当前 WSL 用户已持久化如下模块代理设置：

```bash
go env -w GOPROXY=https://goproxy.cn,direct
```

这会写入当前用户的 Go 配置，不会修改仓库。若某次必须使用企业代理或 Windows 上的本地代理，可临时覆盖：

```bash
HOST_IP="$(ip route | awk '/default/ {print $3; exit}')"
export HTTP_PROXY="http://${HOST_IP}:7890"
export HTTPS_PROXY="http://${HOST_IP}:7890"
export ALL_PROXY="$HTTPS_PROXY"
```

WSL2 的 NAT 模式中，`127.0.0.1:7890` 指向 WSL 自己，不是 Windows 宿主机。因此本地代理应使用默认路由中的 Windows 网关地址。代理只在需要下载依赖、镜像或 envtest 二进制的 shell 中设置；不要把不确定可用的代理永久写进项目脚本。

## 4. Docker Engine、kind 与 kubectl

先检查 Docker daemon 是否在运行：

```bash
docker version --format 'client={{.Client.Version}} server={{.Server.Version}}'
docker info --format 'driver={{.Driver}} cgroup={{.CgroupDriver}}'
```

当前 WSL 没有启用 systemd，因此 Docker daemon 不保证在 WSL 或 Windows 重启后自动恢复。如果客户端提示无法连接 `/var/run/docker.sock`，从**管理员 PowerShell**启动已有的 WSL Docker Engine：

```powershell
wsl.exe -d Ubuntu -u root -- bash -lc 'pgrep -x dockerd >/dev/null || nohup dockerd --host=unix:///var/run/docker.sock >/var/log/dockerd.log 2>&1 &'
```

然后回到普通 WSL 用户核验：

```bash
docker version
kind get clusters
kubectl config current-context
kubectl get nodes -o wide
kubectl get --raw=/readyz; echo
```

本地演示集群的预期名称是 `cricket-e2e`，context 为 `kind-cricket-e2e`。`kubectl get --raw=/readyz` 返回 `ok` 且节点为 `Ready` 后，才应部署或运行平台 E2E。

不要为了修复短暂启动问题直接执行 `kind delete cluster` 或重建 Docker 数据目录：这会删除本地集群、工作负载、镜像缓存或历史观察状态。先收集 `docker ps -a`、`docker logs cricket-e2e-control-plane`、`kubectl get pods -A`，再决定是否需要重建。

当前 Docker build 能成功完成，但输出提示正在使用 legacy builder，且 `docker buildx` 子命令尚未安装。这不阻塞本轮 kind 验证；不过在 CI 或正式交付前，应安装 Buildx 并迁移到 BuildKit，避免将来 Docker 移除 legacy builder 后失去镜像构建能力。

## 5. 不修改工作树的基础校验

在工作树仍有未提交变更时，优先执行以下只读/临时输出检查：

```bash
cd /mnt/d/vGPU/accelerator-control-plane

go test ./... -count=1
go vet ./...
find . -name '*.go' -not -path './vendor/*' -print0 | xargs -0 gofmt -l
go build -trimpath -o /tmp/accelerator-control-plane-manager ./cmd/manager
file /tmp/accelerator-control-plane-manager
rm -f /tmp/accelerator-control-plane-manager

git diff --check
```

2026-07-31 的重新执行结果如下：

| 检查 | 本轮结果 |
|---|---|
| `go test ./... -count=1` | 通过；Adapter、Allocator、Controller、Injector、Inventory、Policy、Local/Cricket Provider 等包均通过 |
| `go vet ./...` | 通过 |
| `go build -trimpath ...` | 通过，生成的临时 Linux x86-64 manager 二进制已删除 |
| `make generate` / `make manifests` | 连续执行两次后 DeepCopy 与五份 CRD bases 哈希一致 |
| `make verify` | 通过；已执行 `go fmt`、vet、全量测试和 `bin/manager` 构建 |
| `make envtest` | 通过；真实 API server/etcd 的 `TestCRDsAndAtomicEndpointClaimsWithEnvtest` 通过 |
| `make container-build` | 通过；当前 `accelerator-control-plane:dev` 为 `sha256:ae222a4e…`，已加载至 kind 并滚动更新 manager |
| kind 集群健康 | 通过；`cricket-e2e-control-plane` 为 `Ready`，`/readyz` 为 `ok` |
| 复杂 Remote/Auto 平台矩阵 | 通过；5 份 CRD dry-run、3 个 RemoteOnly CUDA 图像任务与 1 个 Auto 任务均成功，Lease 均 Released |
| 合成 LocalOnly 容器 | 通过；选择 Local、绑定 Node、注入 fake extended resource，且无 Cricket runtime 注入；清理无残留 |
| `gofmt -l` | 通过；先前唯一的字段对齐差异已确认并格式化 |
| `git diff --check` | 未报告空白符错误；仅有 Windows CRLF 转换提示 |

先前唯一的 `gofmt` 差异仅为 `AcceleratorWorkloadSpec` 两个字段的对齐空格，已经确认属于本次改动并修复。这个结果不改变原则：未来在混合工作树发现格式差异时，仍应先执行 `gofmt -d <file>`，确认来源后再写回。

## 6. 哪些 Make 目标会改文件

`Makefile` 的目标不是都只读：

| 命令 | 行为 | 脏工作树中的建议 |
|---|---|---|
| `make test` | 运行 `go test ./...` | 可直接运行 |
| `make vet` | 运行 `go vet ./...` | 可直接运行 |
| `make envtest` | 运行真实 API server/etcd 测试 | 配好 assets 后可运行 |
| `make build` | 写入 `bin/manager` | 可运行，但会产生/覆盖构建产物 |
| `make verify` | 先运行 `go fmt ./...`，再 vet/test/build | **会改源码，不应在未核对的脏工作树上直接运行** |
| `make generate` | 更新 DeepCopy | 会改 API 生成物 |
| `make manifests` | 更新 CRD YAML | 会改 CRD 生成物 |

本轮在确认唯一格式化差异后，已经通过 `make verify`。仍建议以后在混合工作树中先按下面顺序检查，再让会改写源码或生成物的目标落盘：

```bash
gofmt -d api/v1alpha1/acceleratorworkload_types.go
make generate
make manifests
make verify
git diff --check
```

每步后都应检查 `git diff --stat` 与 `git status --short`，不要用 `git add -A`、`git reset --hard` 或清理命令去处理混合工作树。

## 7. envtest 资产准备

envtest 会启动真实的 Kubernetes API server 和 etcd，并读取仓库 CRD。它需要与当前 Kubernetes 依赖匹配的测试二进制：

```bash
cd /mnt/d/vGPU/accelerator-control-plane

HOST_IP="$(ip route | awk '/default/ {print $3; exit}')"
export HTTP_PROXY="http://${HOST_IP}:7890"
export HTTPS_PROXY="http://${HOST_IP}:7890"

export KUBEBUILDER_ASSETS="$(setup-envtest use 1.36.2 -p path)"
make envtest
```

如果下载已经完成，第二次执行 `setup-envtest use ...` 会直接返回缓存路径。若没有代理，先确认能访问下载站点；若命令长期无输出，不应无限等待。2026-07-31 的首次直连下载约 185 秒后超时；改用 Windows 网关的 7890 代理后，资产已成功缓存到 `/home/qcrs/.local/share/kubebuilder-envtest/k8s/1.36.2-linux-amd64`，并已重新执行 `make envtest` 通过。这说明问题在下载路径/代理，而不是 envtest 或控制面测试本身。

资产下载成功后，可先核验：

```bash
setup-envtest list -i
printf '%s\n' "$KUBEBUILDER_ASSETS"
make envtest
```

## 8. 本地 Docker/Kubernetes E2E 的启动前检查

在重新执行容器或平台矩阵前，至少确认：

```bash
docker version
kind get clusters
kubectl get nodes
kubectl -n accelerator-system get pods -o wide
kubectl get crd | grep accelerator.qcrs.io
```

接下来才运行对应的样例入口，例如：

```bash
./examples/cricket-e2e/run-demo.sh

GO_TEST_MODE=container RUN_AUTO=true \
  ./examples/cricket-e2e/complex-demo/run-platform-matrix.sh

# 不具备本地 GPU 时，额外验证 Local Adapter 的容器注入契约。
# 该脚本只允许在测试集群临时伪造一个扩展资源。
./examples/local-synthetic-e2e/run.sh
```

这些 E2E 会创建/删除 Kubernetes 资源并使用 Docker 镜像，不属于纯工具链检查。运行前要确认 remote Cricket endpoint、端口转发、防火墙和镜像标签都仍是目标实验环境的值；具体网络和数据面准备分别见 `network-two-lan-configuration.zh-CN.md`、`cricket-standalone-remote-call.zh-CN.md` 与 `cricket-runtime-call-and-validation-report.zh-CN.md`。

## 9. 每次 WSL 重启后的最短恢复流程

```powershell
# 管理员 PowerShell：仅当 docker version 在 WSL 中无法连接时执行
wsl.exe -d Ubuntu -u root -- bash -lc 'pgrep -x dockerd >/dev/null || nohup dockerd --host=unix:///var/run/docker.sock >/var/log/dockerd.log 2>&1 &'
```

```bash
# 普通 WSL shell
cd /mnt/d/vGPU/accelerator-control-plane
go version
docker version
kind get clusters
kubectl get --raw=/readyz; echo
```

如果长期使用这台开发机，可在确认不会影响当前 WSL 服务和远端 Cricket 进程的维护窗口，再单独评估启用 WSL systemd、`systemctl enable docker` 或 Docker Desktop WSL 集成。那是宿主机运行方式的持久变更，不应和当前控制面代码提交混在一起。

## 10. 交付前的环境证据

提交/发布前的测试记录至少应写明：

1. WSL 发行版、Go/Docker/kind/kubectl 版本；
2. `make verify`、envtest、CRD 生成检查和 server-side dry-run 的命令与结果；
3. controller、Cricket client runtime 和业务镜像的 tag 或 digest；
4. RemoteOnly/Auto CUDA 数值校验，以及 Local Adapter 容器契约的独立结果；
5. kind/目标集群 context、远端 Endpoint 版本和实验日期；
6. 工作树对应的 Git commit，而不是只描述“本地已经跑过”。

这样可以把“开发环境已恢复”与“某个可复现交付版本已冻结”区分开：前者已经完成，后者仍需在核对当前混合工作树后按提交范围推进。
