param(
    [string]$Distro = "Ubuntu-22.04",
    [string]$CricketUser = "pc",
    [string]$CricketRoot = "/home/pc/src/cricket",
    [string]$ListenAddress = "192.168.5.6",
    [int]$ListenPort = 53773,
    [int]$RpcbindPort = 111,
    [int]$SshListenPort = 2222,
    [int]$SshWslPort = 22,
    [string]$RemoteAddress = "192.168.5.3",
    [string]$WslLauncher = "/mnt/c/cricket-remote/start-cricket-server.sh"
)

$ErrorActionPreference = "Stop"

function Invoke-WslText {
    param(
        [string]$Command,
        [switch]$Root
    )

    $arguments = @("-d", $Distro)
    if ($Root) {
        $arguments += @("-u", "root")
    }
    $arguments += @("--", "bash", "-lc", $Command)
    $result = & wsl.exe @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "WSL command failed ($LASTEXITCODE): $Command`n$result"
    }
    return (($result -join "`n").Trim())
}

$principal = [Security.Principal.WindowsPrincipal]::new(
    [Security.Principal.WindowsIdentity]::GetCurrent()
)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run this script from an elevated Administrator PowerShell."
}

if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) {
    throw "wsl.exe was not found."
}
if (-not (Get-Command netsh.exe -ErrorAction SilentlyContinue)) {
    throw "netsh.exe was not found."
}

$wslAddresses = Invoke-WslText "hostname -I"
$wslIp = ($wslAddresses -split '\s+' |
    Where-Object { $_ -match '^\d+\.\d+\.\d+\.\d+$' } |
    Select-Object -First 1)
if ($wslIp -notmatch '^\d+\.\d+\.\d+\.\d+$') {
    throw "Could not determine the WSL IPv4 address. Actual output: $wslIp"
}

$gpu = Invoke-WslText "nvidia-smi -L"
if ($gpu -notmatch 'GPU') {
    throw "No NVIDIA GPU was detected in the remote WSL distribution: $gpu"
}

$sshd = Invoke-WslText "command -v sshd || true"
if (-not $sshd) {
    throw "OpenSSH Server is not installed in $Distro. Install it with: apt install openssh-server"
}
Invoke-WslText "service ssh start >/dev/null 2>&1 || service ssh restart >/dev/null 2>&1" -Root | Out-Null
$sshSocket = Invoke-WslText "ss -lnt | grep -E '[:.]$SshWslPort[[:space:]]' || true"
if (-not $sshSocket) {
    throw "sshd did not listen on WSL TCP port $SshWslPort."
}

Invoke-WslText "pgrep -x rpcbind >/dev/null || (nohup rpcbind -f -w >/var/log/rpcbind.log 2>&1 </dev/null & )" -Root | Out-Null

$serverOutput = & wsl.exe -d $Distro -u $CricketUser -- bash $WslLauncher $CricketRoot
if ($LASTEXITCODE -ne 0) {
    throw "WSL Cricket launcher failed ($LASTEXITCODE): $serverOutput"
}

$cricketPort = $null
for ($attempt = 1; $attempt -le 30; $attempt++) {
    Start-Sleep -Milliseconds 500
    $rpcTable = Invoke-WslText "rpcinfo -p 127.0.0.1 2>/dev/null || true"
    $line = $rpcTable -split "`n" |
        Where-Object { $_ -match '^\s*99\s+1\s+tcp\s+(\d+)(?:\s+.*)?$' } |
        Select-Object -First 1
    if ($line -and $line -match '^\s*99\s+1\s+tcp\s+(\d+)(?:\s+.*)?$') {
        $cricketPort = [int]$Matches[1]
        break
    }
}
if (-not $cricketPort) {
    $log = Invoke-WslText "tail -n 80 '$CricketRoot/cricket-server.log' 2>/dev/null || true"
    throw "Cricket RPC server did not register with rpcbind.`n$log"
}

Set-Service -Name iphlpsvc -StartupType Automatic
Start-Service -Name iphlpsvc

$listenInterface = Get-NetIPAddress -AddressFamily IPv4 -IPAddress $ListenAddress -ErrorAction SilentlyContinue |
    Select-Object -First 1
if (-not $listenInterface) {
    throw "Windows does not own listen address $ListenAddress. Check the LAN DHCP reservation/static lease."
}
$profile = Get-NetConnectionProfile -InterfaceIndex $listenInterface.InterfaceIndex -ErrorAction SilentlyContinue
if ($profile -and $profile.NetworkCategory -eq 'Public') {
    Set-NetConnectionProfile -InterfaceIndex $listenInterface.InterfaceIndex -NetworkCategory Private
}

netsh.exe interface portproxy delete v4tov4 listenaddress=$ListenAddress listenport=$ListenPort 2>$null | Out-Null
netsh.exe interface portproxy add v4tov4 listenaddress=$ListenAddress listenport=$ListenPort connectaddress=$wslIp connectport=$cricketPort | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "Failed to create Cricket portproxy rule."
}
netsh.exe interface portproxy delete v4tov4 listenaddress=$ListenAddress listenport=$RpcbindPort 2>$null | Out-Null
netsh.exe interface portproxy add v4tov4 listenaddress=$ListenAddress listenport=$RpcbindPort connectaddress=$wslIp connectport=$RpcbindPort | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "Failed to create rpcbind portproxy rule."
}
netsh.exe interface portproxy delete v4tov4 listenaddress=$ListenAddress listenport=$SshListenPort 2>$null | Out-Null
netsh.exe interface portproxy add v4tov4 listenaddress=$ListenAddress listenport=$SshListenPort connectaddress=$wslIp connectport=$SshWslPort | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "Failed to create SSH portproxy rule."
}

$rules = @(
    @{ Name = "Cricket RPC $ListenPort from local host"; Port = $ListenPort },
    @{ Name = "Cricket rpcbind $RpcbindPort from local host"; Port = $RpcbindPort },
    @{ Name = "Cricket Remote WSL SSH from $RemoteAddress"; Port = $SshListenPort }
)
foreach ($rule in $rules) {
    if (-not (Get-NetFirewallRule -DisplayName $rule.Name -ErrorAction SilentlyContinue)) {
        New-NetFirewallRule -DisplayName $rule.Name -Direction Inbound -Action Allow -Protocol TCP `
            -LocalAddress $ListenAddress -LocalPort $rule.Port -RemoteAddress $RemoteAddress -Profile Private | Out-Null
    }
}

$tcpReady = Test-NetConnection -ComputerName $ListenAddress -Port $ListenPort `
    -InformationLevel Quiet -WarningAction SilentlyContinue
if (-not $tcpReady) {
    throw "Fixed Cricket endpoint $ListenAddress`:$ListenPort is not listening."
}
$sshReady = Test-NetConnection -ComputerName $ListenAddress -Port $SshListenPort `
    -InformationLevel Quiet -WarningAction SilentlyContinue
if (-not $sshReady) {
    throw "Fixed SSH endpoint $ListenAddress`:$SshListenPort is not listening."
}

Write-Host "Cricket ready" -ForegroundColor Green
Write-Host "GPU: $($gpu -split "`n" | Select-Object -First 1)"
Write-Host "WSL IP: $wslIp"
Write-Host "Cricket internal port: $cricketPort"
Write-Host "Fixed endpoint: $ListenAddress`:$ListenPort"
Write-Host "Fixed SSH endpoint: $ListenAddress`:$SshListenPort -> WSL port $SshWslPort"
Write-Host "Run locally with: REMOTE_GPU_ADDRESS=$ListenAddress`:$ListenPort  CRICKET_RPCID=1"
Write-Host "SSH from local WSL with: ssh -p $SshListenPort $CricketUser@$ListenAddress"
Write-Host "Portproxy:"
netsh.exe interface portproxy show v4tov4
