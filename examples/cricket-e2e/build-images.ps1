[CmdletBinding()]
param(
    [string]$RuntimeImage = "cricket-client:5d55bd5",
    [string]$WorkloadImage = "cricket-remote-kernel-test:cuda12.8",
    [string]$ManagerImage = "accelerator-control-plane:dev",
    [string]$KindCluster = ""
)

$ErrorActionPreference = "Stop"

$exampleRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$repositoryRoot = Resolve-Path (Join-Path $exampleRoot "..\..")

& (Join-Path $exampleRoot "prepare-runtime.ps1")

docker build -t $RuntimeImage (Join-Path $exampleRoot "runtime")
if ($LASTEXITCODE -ne 0) { throw "Cricket runtime image build failed." }

docker build -t $WorkloadImage (Join-Path $exampleRoot "app")
if ($LASTEXITCODE -ne 0) { throw "Workload image build failed." }

docker build -t $ManagerImage $repositoryRoot
if ($LASTEXITCODE -ne 0) { throw "Control-plane image build failed." }

if ($KindCluster) {
    kind load docker-image --name $KindCluster $RuntimeImage $WorkloadImage $ManagerImage
    if ($LASTEXITCODE -ne 0) { throw "Loading images into kind failed." }
}

docker image inspect $RuntimeImage $WorkloadImage $ManagerImage |
    Select-Object -ExpandProperty RepoTags
