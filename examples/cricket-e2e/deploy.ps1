[CmdletBinding()]
param(
    [string]$KubeContext = "",
    [string]$Endpoint = "192.168.5.6",
    [int]$Port = 53773
)

$ErrorActionPreference = "Stop"

$exampleRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$repositoryRoot = Resolve-Path (Join-Path $exampleRoot "..\..")
$sample = Join-Path $repositoryRoot "config\samples\remote-cricket-5060ti.yaml"

if (-not (Test-NetConnection -ComputerName $Endpoint -Port $Port -InformationLevel Quiet)) {
    throw "The Cricket endpoint ${Endpoint}:$Port is not reachable from this host."
}

$contextArgs = @()
if ($KubeContext) {
    $contextArgs = @("--context", $KubeContext)
}

kubectl @contextArgs apply --server-side --force-conflicts -k (Join-Path $repositoryRoot "config\default")
if ($LASTEXITCODE -ne 0) { throw "Control-plane deployment failed." }

kubectl @contextArgs -n accelerator-system rollout status deployment/accelerator-manager --timeout=180s
if ($LASTEXITCODE -ne 0) { throw "Control-plane manager did not become ready." }

$sampleWorkload = "remote-kernel-test"
# AcceleratorWorkload.spec is immutable after allocation starts. The sample
# manifest contains this runnable smoke workload, so reset only this named
# example before a repeated demo deployment.
kubectl @contextArgs -n default delete "acceleratorworkload/$sampleWorkload" --ignore-not-found --wait=true --timeout=120s
if ($LASTEXITCODE -ne 0) { throw "Existing Cricket sample workload could not be reset." }

kubectl @contextArgs apply -f $sample
if ($LASTEXITCODE -ne 0) { throw "Cricket E2E objects failed to apply." }

kubectl @contextArgs -n default wait "acceleratorworkload/$sampleWorkload" --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
if ($LASTEXITCODE -ne 0) { throw "Cricket sample workload did not succeed." }

$sampleLease = (kubectl @contextArgs -n default get "acceleratorworkload/$sampleWorkload" -o "jsonpath={.status.allocationRef}").Trim()
if ($LASTEXITCODE -ne 0) { throw "Cricket sample workload status could not be read." }
if ($sampleLease) {
    kubectl @contextArgs -n default wait "acceleratorlease/$sampleLease" --for=jsonpath='{.status.phase}'=Released --timeout=120s
    if ($LASTEXITCODE -ne 0) { throw "Cricket sample lease was not released." }
}

kubectl @contextArgs get acceleratorprovider,acceleratorpool,acceleratorendpoint
kubectl @contextArgs -n default get acceleratorworkload,acceleratorlease,pod
