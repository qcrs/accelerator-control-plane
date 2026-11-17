# 远端 Windows + WSL Cricket 启动与恢复说明

本文说明远端 GPU 电脑上的三个交付文件分别负责什么、首次如何放置、每次开机如何启动，以及怎样确认远程 GPU 服务已经就绪。

## 1. 使用场景

当前远端电脑配置为：

```text
Windows LAN IP: 192.168.5.6
WSL distribution: Ubuntu-22.04
WSL user: pc
WSL Cricket root: /home/pc/src/cricket
GPU: NVIDIA GeForce RTX 5060 Ti 16 GB
CUDA Toolkit: 12.8
cuDNN: 9.12
Fixed LAN endpoint: 192.168.5.6:53773
Fixed SSH endpoint: 192.168.5.6:2222
Allowed client Windows IP: 192.168.5.3
```

WSL 的内部 IP 会在 Windows/WSL 重启后变化，Cricket RPC Server 每次启动时注册的内部 TCP 端口也可能变化。因此，客户端不能直接保存 WSL IP 或 Cricket 内部端口。

对客户端长期保持不变的地址是：

```text
Cricket RPC: 192.168.5.6:53773
WSL SSH:     192.168.5.6:2222
```

启动脚本会把 Cricket 固定入口自动转发到本次实际的 WSL IP 和 Cricket 动态端口，并把 SSH 固定入口转发到本次 WSL 的 TCP 22。远端重启后不需要手工修改 SSH 的 `portproxy`。

## 2. 远端目录中的三个文件

远端 Windows 必须保留以下三个文件：

```text
C:\cricket-remote\start-cricket.ps1
C:\cricket-remote\start-cricket-server.sh
C:\cricket-remote\README.zh-CN.md
```

### 2.1 `start-cricket.ps1`

这是用户日常执行的主入口，运行在远端 Windows 管理员 PowerShell 中。

它负责：

1. 检查管理员权限、`wsl.exe` 和 `netsh.exe`；
2. 启动 WSL2 并获取当前 WSL IPv4 地址；
3. 通过 `nvidia-smi -L` 检查 GPU；
4. 以 WSL root 用户启动或复用 `rpcbind`；
5. 调用 `start-cricket-server.sh` 启动 Cricket；
6. 从 `rpcinfo` 自动发现 Cricket 本次内部端口；
7. 启动 Windows IP Helper 服务并检查网络类型；
8. 更新 Windows `portproxy`；
9. 创建仅允许 `192.168.5.3` 访问的防火墙规则；
10. 启动 WSL `sshd`，刷新 `192.168.5.6:2222 -> WSL:22`；
11. 验证 Cricket 和 SSH 两个固定入口都已监听。

不要在普通 PowerShell 或 WSL shell 中直接执行这个文件。

### 2.2 `start-cricket-server.sh`

这是 WSL 内部启动器，由 `start-cricket.ps1` 自动调用，用户通常不需要单独运行。

它负责：

1. 检查 `/home/pc/src/cricket/bin/cricket-rpc-server`；
2. 停止同一安装目录下的旧 Cricket Server；
3. 设置 Cricket、libtirpc、CUDA 和 WSL 驱动库的动态链接路径；
4. 设置 `CUDA_VISIBLE_DEVICES=0`；
5. 使用 `nohup` 在后台启动 Cricket；
6. 把日志写入 `/home/pc/src/cricket/cricket-server.log`；
7. 检查新进程没有在启动阶段退出。

PowerShell 和 Bash 启动器分离，是为了避免 Windows PowerShell 5.1 向 `bash -lc` 传递复杂命令时发生转义错误。

### 2.3 `README.zh-CN.md`

即本文，是远端机器随附的操作说明。它不参与运行，可以直接用记事本、VS Code 或 Markdown 阅读器打开。

## 3. 首次上传到远端

三个文件在本地仓库中的位置是：

```text
examples/cricket-e2e/remote/
```

通过临时 HTTP、U 盘、共享目录、SCP 或其他可信方式，把三个文件放到：

```text
C:\cricket-remote
```

临时 HTTP 传输的完整步骤见本地文件：

```text
examples/cricket-e2e/remote/TRANSFER.zh-CN.md
```

上传后检查：

```powershell
Get-ChildItem C:\cricket-remote
```

应看到：

```text
README.zh-CN.md
start-cricket.ps1
start-cricket-server.sh
```

## 4. 每次远端电脑开机后的启动命令

打开远端 Windows 的“管理员 PowerShell”，执行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File C:\cricket-remote\start-cricket.ps1
```

不需要预先进入 WSL，不需要手工启动 `rpcbind`，也不需要查询 WSL IP 或 Cricket 内部端口。

成功输出示例：

```text
Cricket ready
GPU: GPU 0: NVIDIA GeForce RTX 5060 Ti (...)
WSL IP: 172.29.184.27
Cricket internal port: 55599
Fixed endpoint: 192.168.5.6:53773
Fixed SSH endpoint: 192.168.5.6:2222 -> WSL port 22
Run locally with: REMOTE_GPU_ADDRESS=192.168.5.6:53773  CRICKET_RPCID=1
SSH from local WSL with: ssh -p 2222 pc@192.168.5.6
```

随后应显示类似的转发表：

```text
192.168.5.6  53773  172.29.184.27  55599
192.168.5.6  111    172.29.184.27  111
192.168.5.6  2222   172.29.184.27  22
```

其中 WSL IP 和 Cricket internal port 允许变化；`192.168.5.6:53773` 必须保持不变。

脚本可以重复执行。重复执行会重启 Cricket 并刷新转发，不需要手工删除旧规则。

## 5. 启动后的远端检查

检查固定入口：

```powershell
Test-NetConnection 192.168.5.6 -Port 53773
Test-NetConnection 192.168.5.6 -Port 2222
```

预期：

```text
TcpTestSucceeded : True
```

检查 Windows 转发：

```powershell
netsh interface portproxy show v4tov4
```

检查 WSL 进程：

```powershell
wsl -d Ubuntu-22.04 -- bash -lc "pgrep -af 'rpcbind|cricket-rpc-server'"
```

检查 RPC 注册：

```powershell
wsl -d Ubuntu-22.04 -- rpcinfo -p 127.0.0.1
```

应包含 program `99`、version `1`、protocol `tcp` 的记录。

从调用端 WSL 检查 SSH：

```bash
ssh cricket-remote 'hostname; whoami; nvidia-smi -L; nvcc --version'
```

该命令应显示远端主机名、用户 `pc`、RTX 5060 Ti 和 CUDA 12.8。SSH 使用密钥登录；不要启用公网密码登录。

查看 Cricket 日志：

```powershell
wsl -d Ubuntu-22.04 -- bash -lc "tail -n 80 /home/pc/src/cricket/cricket-server.log"
```

## 6. 本地客户端怎样使用

本地独立客户端始终使用：

```text
REMOTE_GPU_ADDRESS=192.168.5.6:53773
CRICKET_RPCID=1
```

不要把 WSL IP 或动态端口写进应用配置；调用端只保存 Windows 固定入口 `192.168.5.6:53773`。

## 7. 参数覆盖

默认参数与当前实验环境一致。如果地址、用户名或安装目录变化，可以覆盖：

```powershell
C:\cricket-remote\start-cricket.ps1 `
  -Distro Ubuntu-22.04 `
  -CricketUser pc `
  -CricketRoot /home/pc/src/cricket `
  -ListenAddress 192.168.5.6 `
  -ListenPort 53773 `
  -SshListenPort 2222 `
  -SshWslPort 22 `
  -RemoteAddress 192.168.5.3
```

如果文件不在 `C:\cricket-remote`，还需要指定 Bash 启动器在 WSL 中可见的路径：

```powershell
-WslLauncher /mnt/c/其他目录/start-cricket-server.sh
```

## 8. 地址管理建议

建议在路由器 DHCP 地址保留中固定：

```text
本地 Windows: 192.168.5.3
远端 Windows: 192.168.5.6
```

如果地址变化，需要同步修改脚本参数、防火墙来源和客户端的 `REMOTE_GPU_ADDRESS`。WSL 内部 `172.x.x.x` 地址不需要固定。

## 9. 常见问题

### 9.1 `Cricket RPC server did not register with rpcbind`

执行：

```powershell
wsl -d Ubuntu-22.04 -- bash -lc "tail -n 80 /home/pc/src/cricket/cricket-server.log"
wsl -d Ubuntu-22.04 -- rpcinfo -p 127.0.0.1
```

检查 Cricket 可执行文件、动态库和 `rpcbind`。

### 9.2 本机端口成功，局域网客户端不通

执行：

```powershell
Get-NetConnectionProfile
Get-NetFirewallRule | Where-Object DisplayName -Match 'Cricket'
netsh interface portproxy show v4tov4
```

确认承载 `192.168.5.6` 的网络为 Private，且允许来源仍是 `192.168.5.3`。

### 9.3 输出 `unknown format: 0x2`

这是当前 Cricket ELF 解析器面对新 CUDA 产物时的兼容性警告。如果同时满足：

```text
kernel launch: err=0
kernel sync: err=0
verification failures: 0
```

可以作为非阻塞警告记录；如果 kernel 或数值校验失败，则不能忽略。

## 10. 安全边界

Cricket 当前没有 TLS 和身份认证。服务只应暴露在可信私有局域网或 VPN 中。

脚本创建的防火墙规则仅允许 `192.168.5.3` 访问 `111`、`2222` 和 `53773`。不要改成对任意来源开放，也不要把端口映射到公网 WAN。SSH 应保持 `PasswordAuthentication no`、`PermitRootLogin no`，并妥善保管调用端私钥。
