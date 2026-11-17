[CmdletBinding()]
param(
    [string]$ComplexImage = "cricket-image-demos:cuda12.8",
    [string]$KindCluster = ""
)

$ErrorActionPreference = "Stop"
$exampleRoot = Split-Path -Parent $MyInvocation.MyCommand.Path

docker build -t $ComplexImage (Join-Path $exampleRoot "app")
if ($LASTEXITCODE -ne 0) {
    throw "Complex image demo build failed."
}

if ($KindCluster) {
    kind load docker-image --name $KindCluster $ComplexImage
    if ($LASTEXITCODE -ne 0) {
        throw "Loading complex image into kind failed."
    }
}

docker image inspect $ComplexImage |
    Select-Object -ExpandProperty RepoTags
