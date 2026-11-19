"""Independent replay and final live-state audit for this acceptance run."""
import datetime as dt
import hashlib
import importlib.util
import json
import subprocess
from pathlib import Path

folder = Path(__file__).resolve().parent
root = folder.parents[2]
suite = folder / "k3s-final"
bundle = root / "dist/k3s-correctness-20261119-v2"


def read(path):
    return json.loads(path.read_text())


def require(condition, message):
    if not condition:
        raise ValueError(message)


def get(resource, *args):
    return json.loads(subprocess.check_output(
        ["kubectl", "get", resource, *args, "-o", "json", "--request-timeout=10s"]))


def image_ids(pod, expected):
    spec, status = pod["spec"], pod["status"]
    statuses = {c["name"]: c for key in ("initContainerStatuses", "containerStatuses") for c in status.get(key, [])}
    result = []
    for key in ("initContainers", "containers"):
        for container in spec.get(key, []):
            tag = container["image"].removeprefix("docker.io/library/")
            digest = statuses.get(container["name"], {}).get("imageID")
            require(tag in expected and digest == expected[tag]["configDigest"], "image identity: " + pod["metadata"]["name"])
            result.append({"image": container["image"], "configDigest": digest})
    return result


def audit():
    summary, info = read(suite / "summary.json"), read(bundle / "bundle.json")
    require(summary["result"] == "PASS" and len(summary["scenarios"]) == 13 and summary["disruptionsExecuted"], "full 13-scenario matrix did not pass")
    namespace = summary["namespace"]
    loader = importlib.util.spec_from_file_location("smoke_verifier", root / "examples/remote-real-smoke/verify.py")
    verifier = importlib.util.module_from_spec(loader)
    loader.loader.exec_module(verifier)
    successes, expected_failures, deleted = [], [], []
    live_workloads = get("acceleratorworkloads", "-A")["items"]
    live_leases = get("acceleratorleases", "-A")["items"]
    live_pods = get("pods", "-n", namespace)["items"]
    work_names = {o["metadata"]["name"] for o in live_workloads if o["metadata"]["namespace"] == namespace}
    lease_names = {o["metadata"]["name"] for o in live_leases if o["metadata"]["namespace"] == namespace}
    pod_names = {o["metadata"]["name"] for o in live_pods}
    captured = sorted(suite.glob("*/pod.json"))
    require(len(captured) == 15, "expected 15 captured worker Pods")
    for path in captured:
        case = path.parent
        pod, workload, lease = read(path), read(case / "workload.json"), read(case / "lease.json")
        require(all(o["metadata"]["namespace"] == namespace for o in (pod, workload, lease)), "namespace mismatch: " + case.name)
        require(workload["status"]["podRef"]["uid"] == pod["metadata"]["uid"], "Workload Pod UID mismatch")
        require(lease["status"]["podRef"]["uid"] == pod["metadata"]["uid"], "Lease Pod UID mismatch")
        require(lease["spec"]["owner"]["uid"] == workload["metadata"]["uid"], "Lease owner UID mismatch")
        require(any(o["uid"] == workload["metadata"]["uid"] and o.get("controller") for o in pod["metadata"]["ownerReferences"]), "Pod controller UID mismatch")
        require(not pod["spec"].get("hostPID") and not pod["spec"].get("hostNetwork") and not any("hostPath" in v for v in pod["spec"].get("volumes", [])), "client host isolation failed")
        image_ids(pod, info["images"])
        if (case / "verification.json").exists():
            evidence = read(case / "verification.json")
            verifier.verify(case, evidence["driver"], now=dt.datetime.fromisoformat(evidence["verifiedAt"]))
            for field, obj in (("workloadUID", workload), ("podUID", pod), ("leaseUID", lease)):
                require(evidence[field] == obj["metadata"]["uid"], "verification UID mismatch: " + field)
            require("LOCAL_ACCELERATOR_DEVICES=NONE" in (case / "client.stdout").read_text(), "missing local device absence proof")
            successes.append(case.name)
        elif workload["status"]["phase"] == "Failed":
            fallback = workload["status"].get("fallback", {})
            require(fallback.get("readyObserved") and not fallback.get("history") and not fallback.get("failedAttempts"), "post-Ready failure was replayed")
            require(workload["status"]["allocationAttempt"] == 1 and lease["status"]["phase"] == "Released", "post-Ready allocation was replaced or not released")
            worker = next(c for c in pod["status"]["containerStatuses"] if c["name"] == "worker")
            require(worker["state"]["terminated"]["exitCode"] == 1, "unexpected post-Ready failure exit code")
            expected_failures.append(case.name)
        else:
            require(workload["metadata"]["name"] not in work_names and lease["metadata"]["name"] not in lease_names and pod["metadata"]["name"] not in pod_names, "running deletion left resources behind")
            deleted.append(case.name)
    require((len(successes), len(expected_failures), len(deleted)) == (11, 2, 2), "unexpected numerical/fault/deletion counts")
    require(all(o.get("status", {}).get("phase") in ("Succeeded", "Failed") for o in live_workloads), "active platform Workload remains")
    require(all(o.get("status", {}).get("phase") in ("Released", "Failed", "Expired") for o in live_leases), "active platform Lease remains")
    claims = get("leases", "-n", "accelerator-system", "-l", "accelerator.qcrs.io/claim-type=remote-endpoint")["items"]
    require(not claims, "Endpoint Claims remain")
    endpoints = get("acceleratorendpoints")["items"]
    configurations = read(bundle / "configuration.json")["items"]
    original = next(o["spec"]["address"] for o in configurations if o["kind"] == "AcceleratorEndpoint" and o["metadata"]["name"] == "corex-remote-server")
    stamp = namespace.removeprefix("accelerator-validation-")
    require(not any(o["metadata"]["name"] == "corex-nat-" + stamp for o in get("services", "-n", "accelerator-system")["items"]), "temporary Service remains")
    now = dt.datetime.now(dt.timezone.utc)
    for endpoint in endpoints:
        status = endpoint["status"]
        require(status["health"] == "Healthy" and status["confidence"] == "Observed" and status["activeSessions"] == 0 and status.get("reserved", 0) == 0 and status["available"] == 1, "Endpoint is not observed idle")
        age = (now - dt.datetime.fromisoformat(status["observedAt"].replace("Z", "+00:00"))).total_seconds()
        require(0 <= age <= 60, "final Endpoint observation is stale")
        ready = next(c for c in status["conditions"] if c["type"] == "Ready")
        require(ready["status"] == "True" and ready["observedGeneration"] == endpoint["metadata"]["generation"], "final Ready condition is stale")
        if endpoint["metadata"]["name"] == "corex-remote-server":
            require(endpoint["spec"]["address"] == original, "original CoreX DNS was not restored")
    system = []
    for pod in get("pods", "-n", "accelerator-system")["items"]:
        if pod["metadata"].get("labels", {}).get("app.kubernetes.io/name") not in ("accelerator-control-plane", "corex-remote-server", "mlu-remote-server"):
            continue
        require(all(c["ready"] for c in pod["status"]["containerStatuses"]), "system Pod not Ready")
        system.append({"pod": pod["metadata"]["name"], "images": image_ids(pod, info["images"])})
    require(len(system) == 4, "expected two manager and two runtime server Pods")
    for resource, replicas in (("deployment/accelerator-manager", 2), ("deployment/mlu-remote-server", 1), ("statefulset/corex-remote-server", 1)):
        obj = get(resource, "-n", "accelerator-system")
        require(obj["spec"]["replicas"] == replicas and obj["status"].get("readyReplicas", 0) == replicas, "replicas not restored: " + resource)
    lifecycle = read(folder / "lifecycle-summary.json")
    require(not lifecycle["violations"], "Endpoint status invariant violations were sampled")
    snapshot = read(folder / "source-snapshot.json")
    require(all(hashlib.sha256((root / name).read_bytes()).hexdigest() == digest for name, digest in snapshot["sourceFileSHA256"].items()), "source changed after the tested image was built")
    (suite / "final-endpoints.json").write_text(json.dumps(endpoints, indent=2) + "\n")
    return {"result": "PASS", "auditedAt": now.isoformat(), "scenarios": 13,
            "namespace": namespace, "successfulNumericalWorkloads": successes,
            "expectedPostReadyFailures": expected_failures, "confirmedRunningDeletions": deleted,
            "capturedPodCount": len(captured), "allCapturedRuntimeConfigDigestsMatch": True,
            "systemImages": system, "allPlatformTasksTerminal": True, "endpointClaimsRemaining": 0,
            "originalCorexAddressRestored": True, "temporaryServiceRemoved": True,
            "sourceHashesUnchanged": True, "lifecycle": lifecycle,
            "physicalTopology": "one ARM node; clients without devices/SDK hostPath; real CoreX GPU and MLU NPU servers"}


if __name__ == "__main__":
    try:
        report = audit()
    except (ValueError, KeyError, OSError, StopIteration, subprocess.SubprocessError) as error:
        report = {"result": "FAIL", "error": str(error)}
    (suite / "audit.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({k: v for k, v in report.items() if k not in ("systemImages", "lifecycle")}, indent=2))
    raise SystemExit(0 if report["result"] == "PASS" else 1)
