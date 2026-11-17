[CmdletBinding()]
param(
    [string]$KubeContext = ""
)

$ErrorActionPreference = "Stop"

$contextArgs = @()
if ($KubeContext) {
    $contextArgs = @("--context", $KubeContext)
}

kubectl @contextArgs -n default wait acceleratorworkload/remote-kernel-test `
    --for=jsonpath='{.status.phase}'=Succeeded `
    --timeout=180s

$podName = kubectl @contextArgs -n default get acceleratorworkload remote-kernel-test `
    -o jsonpath='{.status.podRef.name}'
if ($LASTEXITCODE -ne 0 -or -not $podName) {
    throw "The workload did not publish a Pod reference."
}

kubectl @contextArgs -n default get acceleratorworkload remote-kernel-test -o wide
kubectl @contextArgs -n default get acceleratorlease
kubectl @contextArgs -n default logs $podName -c worker
