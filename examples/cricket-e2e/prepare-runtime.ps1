[CmdletBinding()]
param(
    [string]$Distro = "Ubuntu",
    [string]$SourceDirectory = "/home/qcrs/cricket-client"
)

$ErrorActionPreference = "Stop"

$exampleRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$target = Join-Path $exampleRoot "runtime\bin"
New-Item -ItemType Directory -Force $target | Out-Null

$targetPath = [System.IO.Path]::GetFullPath($target)
$targetRoot = [System.IO.Path]::GetPathRoot($targetPath)
if ($targetRoot -notmatch "^[A-Za-z]:\\$") {
    throw "The runtime staging directory must be on a Windows drive."
}
$drive = $targetRoot.Substring(0, 1).ToLowerInvariant()
$relativeTarget = $targetPath.Substring($targetRoot.Length).Replace("\", "/")
$wslTarget = "/mnt/$drive/$relativeTarget"

$copyScript = @"
set -e
test -s '$SourceDirectory/cricket-client.so'
test -s '$SourceDirectory/libtirpc.so.3.0.0'
install -m 0755 '$SourceDirectory/cricket-client.so' '$wslTarget/cricket-client.so'
install -m 0755 '$SourceDirectory/libtirpc.so.3.0.0' '$wslTarget/libtirpc.so.3.0.0'
cp -f '$wslTarget/libtirpc.so.3.0.0' '$wslTarget/libtirpc.so.3'
cp -f '$wslTarget/libtirpc.so.3.0.0' '$wslTarget/libtirpc.so'
"@

& wsl.exe -d $Distro -- bash -lc $copyScript
if ($LASTEXITCODE -ne 0) {
    throw "Could not stage the Cricket client runtime from WSL."
}

Get-ChildItem $target |
    Select-Object Name, Length, LastWriteTime
