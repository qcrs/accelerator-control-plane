# 双局域网异构节点网络配置报告

## 1. 文档定位

本文面向网络管理员、平台运维人员和现场实施人员，说明 Kubernetes 控制面节点与远端 GPU 节点之间如何建立可复现的局域网连接。

本文覆盖以下组合：

- 两台 Windows，远端 GPU 运行在 WSL2；
- 两台 Linux 服务器；
- 一台 Linux 与一台 Windows/WSL2；
- 两台机器连接同一个路由器；
- 两台机器位于不同网段时的额外要求。

本文只讨论 Cricket 所需的内网连接，不讨论公网暴露。当前 Cricket 客户端按 IPv4 验证，正式配置优先使用 IPv4。

## 2. 推荐拓扑

当前已验证的拓扑如下：

```text
                         LAN 192.168.5.0/24

             Router LAN: 192.168.5.1
                    /                    \
                   /                      \
  本地 Windows / WSL                远端 Windows / WSL
  Windows: 192.168.5.3              Windows: 192.168.5.6
  Kubernetes/kind                   RTX 5060 Ti
                                    WSL2: 172.29.x.x
                                    rpcbind: TCP 111
                                    Cricket: TCP 53773
```

这里的“本地”和“远端”是业务角色，不代表一定要使用不同操作系统：

- 本地节点运行 Kubernetes、Controller、业务 Pod 和测试命令；
- 远端节点运行 NVIDIA 驱动、CUDA、`rpcbind` 和 `cricket-rpc-server`；
- 远端 Windows 主机负责把稳定的 LAN 地址 `192.168.5.6` 转发到动态 WSL 地址；
- 业务 Pod 通过 `192.168.5.6:53773` 访问远端 GPU。

### 2.1 地址规划

| 角色 | 推荐地址 | 说明 |
| --- | --- | --- |
| 路由器 LAN | `192.168.5.1/24` | 默认网关、DHCP 服务 |
| 本地 Windows | `192.168.5.3/24` | Kubernetes/kind 所在主机 |
| 远端 Windows | `192.168.5.6/24` | WSL2 GPU 主机的稳定入口 |
| 远端 WSL2 | `172.29.x.x/20` | 动态 NAT 内部地址，不写入 Endpoint |
| rpcbind | `192.168.5.6:111` | Cricket 服务登记端口 |
| Cricket RPC | `192.168.5.6:53773` | 当前验证时的服务端口 |

`192.168.5.6` 和 `53773` 是当前环境的示例值。正式部署应以现场实际地址和 `rpcinfo -p` 查询结果为准。

## 3. 路由器配置原则

### 3.1 同一个路由器、同一个网段

如果两台机器都连接到同一个路由器的普通 LAN/WLAN，并且都获得 `192.168.5.x/24` 地址：

- 不需要手工配置静态路由；
- 不需要配置 WAN 端口转发；
- 不需要公网 IP；
- 不需要 IPv6；
- 只需要确保二层客户端之间允许互访。

### 3.2 推荐 DHCP 地址保留

推荐在路由器的 DHCP 页面做“地址保留”“静态租约”或“IP-MAC 绑定”，而不是在 Windows/Linux 网卡上手工写死地址。

建议保留：

```text
本地控制主机 MAC -> 192.168.5.3
远端 GPU 主机 MAC -> 192.168.5.6
```

优点是：

- 地址由路由器统一管理；
- 重启、换网卡状态后地址仍稳定；
- 不会和 DHCP 地址池冲突；
- 网关和 DNS 仍由 DHCP 正确下发。

如果必须在系统中使用静态 IPv4，应选择 DHCP 地址池之外的地址，并设置：

```text
地址:     192.168.5.3 或 192.168.5.6
掩码:     255.255.255.0
网关:     192.168.5.1
DNS:      192.168.5.1 或现场 DNS
```

同一台机器不要同时使用“系统静态地址”和“路由器 DHCP 保留”作为两套来源。

### 3.3 无线与 LAN 页面

不同路由器名称不同，按功能查找并确认：

- 远端设备连接的是普通主 SSID，不是 Guest/访客网络；
- “无线客户端隔离”“AP Isolation”“SSID 隔离”关闭；
- “LAN 侧访问”“本地服务控制”允许 LAN 客户端互访；
- 防火墙允许 LAN 到 LAN 的 TCP 连接；
- 如果路由器有 VLAN/多 SSID，控制面和远端 GPU 必须位于可路由的同一 VLAN，或配置明确的三层路由；
- 不要开启只允许访问 Internet、禁止访问内网的访客模式。

当前网络需要的不是 AP 模式，而是同一局域网内的客户端互通。路由器是否显示“AP”菜单不影响本方案，只要客户端隔离关闭、LAN 访问允许即可。

### 3.4 IPv4 与 IPv6

当前 Cricket 适配器对 IPv6 Endpoint 地址明确不支持，Endpoint 应填写 IPv4 或 IPv4 的 `host:port`：

```yaml
spec:
  address: 192.168.5.6:53773
```

IPv6 可以继续用于其他业务，但不能作为当前 Cricket 数据面的唯一连通方式。

## 4. 两台 Windows，远端使用 WSL2

这是当前已经实际验证过的组合。WSL2 是 NAT 网络，WSL 地址可能在重启后变化，因此不能把 WSL 地址直接写进 `AcceleratorEndpoint`。

### 4.1 远端 WSL 内准备服务

在远端 Windows 的 WSL 中执行：

```bash
cat /etc/os-release
nvidia-smi
command -v rpcbind
command -v /opt/cricket/bin/cricket-rpc-server
```

确认 WSL 能看到 GPU，并准备 `rpcbind`：

```bash
sudo apt update
sudo apt install -y rpcbind
```

如果 WSL 开启了 systemd：

```bash
sudo systemctl enable --now rpcbind
```

如果没有 systemd，可用前台方式启动并由 Windows 任务或脚本托管：

```bash
sudo rpcbind -f -w
```

另开一个 WSL 终端启动 Cricket：

```bash
CUDA_VISIBLE_DEVICES=0 /opt/cricket/bin/cricket-rpc-server 1
```

`1` 是 RPC 版本，必须与 Endpoint 的 `parameters.rpcID` 和 `runtime.apiVersion` 一致。

确认 WSL 内部服务：

```bash
rpcinfo -p 127.0.0.1
ss -lntp | grep -E '(:111|:53773)'
```

WSL 内部 Cricket 服务端口可能因为 `rpcbind` 重新登记而变化。正式部署必须在启动脚本中重新读取端口，不能假定每次都是 `53773`。

### 4.2 Windows 端创建稳定入口

使用“管理员 PowerShell”，将 WSL 动态地址转发到远端 Windows 的稳定地址。下面示例转发 TCP 111 和当前 Cricket 端口：

```powershell
$distro = "Ubuntu-22.04"
$listenAddress = "192.168.5.6"
$wslAddress = (wsl.exe -d $distro -- hostname -I).Trim().Split()[0]

netsh interface portproxy delete v4tov4 listenaddress=$listenAddress listenport=111 | Out-Null
netsh interface portproxy delete v4tov4 listenaddress=$listenAddress listenport=53773 | Out-Null

netsh interface portproxy add v4tov4 `
  listenaddress=$listenAddress listenport=111 `
  connectaddress=$wslAddress connectport=111

netsh interface portproxy add v4tov4 `
  listenaddress=$listenAddress listenport=53773 `
  connectaddress=$wslAddress connectport=53773

New-NetFirewallRule `
  -DisplayName "Cricket rpcbind from local control plane" `
  -Direction Inbound -Action Allow -Protocol TCP `
  -Profile Private -LocalAddress $listenAddress -LocalPort 111 `
  -RemoteAddress 192.168.5.3

New-NetFirewallRule `
  -DisplayName "Cricket RPC from local control plane" `
  -Direction Inbound -Action Allow -Protocol TCP `
  -Profile Private -LocalAddress $listenAddress -LocalPort 53773 `
  -RemoteAddress 192.168.5.3
```

检查配置：

```powershell
netsh interface portproxy show v4tov4
Get-NetTCPConnection -LocalPort 111,53773 -State Listen
Get-NetFirewallRule -DisplayName "Cricket *" | Format-Table DisplayName, Enabled, Profile, Direction, Action
```

WSL IP 变化后必须重新执行这段脚本。交付时应把它做成开机任务或 WSL 启动钩子，而不是依赖人工输入。

### 4.3 本地 Windows/Kubernetes 主机验证

在本地 Windows 管理员 PowerShell 或本地 WSL 中执行：

```powershell
Test-NetConnection 192.168.5.6 -Port 111
Test-NetConnection 192.168.5.6 -Port 53773
```

两个端口都应出现：

```text
TcpTestSucceeded : True
```

如果本地 WSL 中安装了 `rpcinfo`：

```bash
rpcinfo -p 192.168.5.6
timeout 5s rpcinfo -t 192.168.5.6 99 1
```

连接超时通常按以下顺序排查：

1. 远端 WSL 中 `rpcbind` 是否运行；
2. Cricket server 是否运行；
3. `rpcinfo -p 127.0.0.1` 显示的服务端口是否仍为 `53773`；
4. Windows portproxy 的 `connectaddress` 是否仍是当前 WSL IP；
5. Windows 网络配置是否为 Private；
6. Windows 防火墙是否只允许了错误的网卡或错误的来源地址；
7. 路由器是否开启了无线客户端隔离。

## 5. 两台 Linux 服务器

纯 Linux 场景不需要 Windows `portproxy`。推荐让远端 Linux 直接使用固定 LAN 地址，并让 Cricket server 与 `rpcbind` 监听可达地址。

远端 Linux 示例：

```bash
ip -4 -br addr
sudo systemctl enable --now rpcbind
CUDA_VISIBLE_DEVICES=0 /opt/cricket/bin/cricket-rpc-server 1
rpcinfo -p 127.0.0.1
ss -lntp | grep -E '(:111|:53773)'
```

如果使用 UFW，只允许本地控制节点访问：

```bash
sudo ufw allow from 192.168.5.3 to any port 111 proto tcp
sudo ufw allow from 192.168.5.3 to any port 53773 proto tcp
sudo ufw status verbose
```

Endpoint 直接填写远端 Linux 地址：

```yaml
spec:
  address: 192.168.5.6:53773
```

如果服务端口在重启后变化，也必须使用 systemd/启动脚本和探针自动更新端口，不能依靠静态 YAML 永久假定端口不变。

## 6. 一台 Linux 与一台 Windows/WSL2

连接原则只取决于哪一侧运行 WSL：

- Linux 控制面 + Windows/WSL GPU：Linux 直接访问 Windows 稳定地址，Windows 端使用 portproxy；
- Windows 控制面/WSL + Linux GPU：Linux 直接暴露服务端口，Windows/WSL 直接访问 Linux 地址；
- 两边都是原生 Linux：两边直接使用固定 LAN 地址，不使用 portproxy。

无论组合如何，Endpoint 填写的必须是“业务 Pod 能访问的地址”，而不是远端 WSL 的 NAT 地址。

## 7. 不同网段的情况

如果两台机器不是同一网段，例如：

```text
控制面: 192.168.5.0/24
远端 GPU: 192.168.6.0/24
```

此时不能只靠关闭 AP 隔离解决，需要二选一：

- 路由器配置两个网段之间的三层路由和防火墙放行；
- 使用企业 VPN/专用隧道，使两端获得可达的内网地址。

不建议用公网 WAN 端口转发直接暴露 Cricket。Cricket 没有 TLS 和客户端认证，公网转发不满足当前安全边界。

## 8. 交付前验收清单

- [ ] 两台机器使用同一普通 LAN/VLAN，或已配置明确路由/VPN。
- [ ] 本地与远端地址通过 DHCP 保留或受控静态地址固定。
- [ ] 无线客户端隔离关闭，非 Guest 网络。
- [ ] 远端 `nvidia-smi` 正常。
- [ ] 远端 `rpcbind` 正常。
- [ ] 远端 Cricket server 的可见 GPU 与 Endpoint `devices` 一致。
- [ ] TCP 111 从本地节点可达。
- [ ] Cricket RPC 服务端口从本地节点可达。
- [ ] Windows WSL IP 变化后 portproxy 可以自动重建。
- [ ] 防火墙只允许 Kubernetes 节点或管理网段访问，不允许公网访问。
- [ ] Endpoint 地址使用稳定 Windows/Linux 地址，不使用 `172.29.x.x` WSL 地址。
- [ ] 路由器没有为本方案配置 WAN 端口转发。

## 9. 当前已知限制

- WSL2 NAT 地址不是稳定资产，必须自动化 portproxy 更新。
- 上游 Cricket 默认通过 `rpcbind` 注册服务，服务端口可能变化。
- 当前适配器只按 IPv4 验证，IPv6 Endpoint 不受支持。
- 当前配置健康主要是静态 `Configured`，网络 `Ready` 需要独立探针。
- `8000` 等临时文件传输端口不属于 Cricket 正式数据面，不应作为交付依赖。
