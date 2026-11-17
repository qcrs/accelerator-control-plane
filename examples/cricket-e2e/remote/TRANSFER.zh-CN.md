# 从本地电脑向远端 Windows 传输 Cricket 启动文件

本文只保存在本地仓库，用于把远端启动所需的三个文件传到 GPU 电脑。

## 需要传输的文件

本地目录：

```text
D:\vGPU\accelerator-control-plane\examples\cricket-e2e\remote
```

需要上传：

```text
start-cricket.ps1
start-cricket-server.sh
README.zh-CN.md
```

`TRANSFER.zh-CN.md` 是本地传输说明，不要求上传到远端。

如果已经配置固定 SSH 入口 `192.168.5.6:2222`，优先使用本文“方法二：SCP”，无需再启动临时 HTTP 服务。

## 方法一：临时 HTTP 传输

### 1. 本地启动临时只读文件服务

在本地 Windows PowerShell 执行：

```powershell
$remoteFiles = 'D:\vGPU\accelerator-control-plane\examples\cricket-e2e\remote'
python -m http.server 8000 --bind 192.168.5.3 --directory $remoteFiles
```

该窗口会保持运行，传输完成前不要关闭。

如果 Windows 防火墙拦截，在本地管理员 PowerShell 创建仅允许远端电脑访问的临时规则：

```powershell
New-NetFirewallRule `
  -DisplayName 'Temporary Cricket file transfer from 192.168.5.6' `
  -Direction Inbound -Action Allow -Protocol TCP `
  -LocalAddress 192.168.5.3 -LocalPort 8000 `
  -RemoteAddress 192.168.5.6 -Profile Private
```

### 2. 远端下载三个文件

在远端 Windows PowerShell 执行：

```powershell
New-Item -ItemType Directory -Force C:\cricket-remote | Out-Null

Invoke-WebRequest http://192.168.5.3:8000/start-cricket.ps1 `
  -OutFile C:\cricket-remote\start-cricket.ps1

Invoke-WebRequest http://192.168.5.3:8000/start-cricket-server.sh `
  -OutFile C:\cricket-remote\start-cricket-server.sh

Invoke-WebRequest http://192.168.5.3:8000/README.zh-CN.md `
  -OutFile C:\cricket-remote\README.zh-CN.md
```

确认：

```powershell
Get-ChildItem C:\cricket-remote
```

### 3. 远端运行

在远端管理员 PowerShell 执行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File C:\cricket-remote\start-cricket.ps1
```

### 4. 关闭临时服务

回到本地运行 HTTP 服务的窗口，按 `Ctrl+C`。检查端口已经关闭：

```powershell
Get-NetTCPConnection -LocalPort 8000 -State Listen -ErrorAction SilentlyContinue
```

如果创建过临时防火墙规则，删除它：

```powershell
Remove-NetFirewallRule `
  -DisplayName 'Temporary Cricket file transfer from 192.168.5.6'
```

## 方法二：SCP（推荐用于后续更新）

在本地 WSL 中执行：

```bash
cd /mnt/d/vGPU/accelerator-control-plane/examples/cricket-e2e/remote

ssh cricket-remote 'mkdir -p /mnt/c/cricket-remote'
scp start-cricket.ps1 start-cricket-server.sh README.zh-CN.md \
  cricket-remote:/mnt/c/cricket-remote/
```

检查远端文件：

```bash
ssh cricket-remote \
  'ls -lh /mnt/c/cricket-remote/start-cricket.ps1 /mnt/c/cricket-remote/start-cricket-server.sh /mnt/c/cricket-remote/README.zh-CN.md'
```

`start-cricket.ps1` 必须在远端 Windows 管理员 PowerShell 中运行，不能通过普通 SSH 会话获得管理员权限。

## 方法三：其他传输方式

也可以使用 U 盘、可信共享目录或 SCP。无论使用哪种方法，都应保持远端路径为：

```text
C:\cricket-remote\start-cricket.ps1
C:\cricket-remote\start-cricket-server.sh
C:\cricket-remote\README.zh-CN.md
```

不要用聊天软件或编辑器重新复制 Bash 脚本内容，以免自动改变引号或换行；优先传输原始文件。
