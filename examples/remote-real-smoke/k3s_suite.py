#!/usr/bin/env python3
"""Real single-node K3s acceptance; leaves timestamped evidence, never fakes PASS."""
import argparse
import copy
import datetime as dt
import importlib.util
import ipaddress
import json
from pathlib import Path
import subprocess
import time


ROOT = Path(__file__).resolve().parents[2]
SYSTEM = "accelerator-system"


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


prepare = load("k3s_prepare", ROOT / "scripts/prepare-k3s-validation.py")
verifier = load("numerical_verifier", Path(__file__).with_name("verify.py"))


def kubectl(*args, obj=None, timeout=45):
    result = subprocess.run(["kubectl", "--request-timeout=30s", *args],
                            input=None if obj is None else json.dumps(obj),
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if result.returncode:
        raise RuntimeError("kubectl " + " ".join(args) + ": " + result.stderr.strip())
    return result.stdout


def get(resource, namespace=None, absent=False):
    args = ["get", resource, "-o", "json"]
    if namespace:
        args += ["-n", namespace]
    if absent:
        args.append("--ignore-not-found")
    text = kubectl(*args)
    return json.loads(text) if text.strip() else None


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def wait(description, predicate, seconds=240):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(2)
    raise TimeoutError(description)


def observed_idle(endpoint):
    status = endpoint.get("status", {})
    ready = next((c for c in status.get("conditions", []) if c["type"] == "Ready"), {})
    try:
        stamp = dt.datetime.fromisoformat(status["observedAt"].replace("Z", "+00:00"))
        age = (dt.datetime.now(dt.timezone.utc) - stamp).total_seconds()
    except (KeyError, ValueError, TypeError):
        return False
    return (status.get("health") == "Healthy" and status.get("confidence") == "Observed"
            and status.get("observationSource") == "remote-probe" and status.get("activeSessions") == 0
            and status.get("reserved", 0) == 0 and 0 <= age <= 60 and ready.get("status") == "True"
            and ready.get("observedGeneration") == endpoint["metadata"]["generation"])


class Suite:
    def __init__(self, folder, bundle):
        self.folder, self.bundle = folder, bundle
        self.started_at = dt.datetime.now(dt.timezone.utc).isoformat()
        self.namespace = "accelerator-validation-" + dt.datetime.now(dt.timezone.utc).strftime("%m%d%H%M%S")
        self.names = []
        self.results = []
        self.folder.mkdir(parents=True, exist_ok=False)
        self.templates = {}
        for driver in ("corex", "mlu"):
            self.templates[driver] = next(o for o in prepare.sample(ROOT / f"config/samples/{driver}-remote.yaml")
                                          if o["kind"] == "AcceleratorWorkload")

    def preflight(self):
        prepare.check_images(self.bundle)
        expected = json.loads((self.bundle / "bundle.json").read_text())["images"]
        actual = get("deployment/accelerator-manager", SYSTEM)
        image = next(c["image"] for c in actual["spec"]["template"]["spec"]["containers"] if c["name"] == "manager")
        if image not in expected:
            raise ValueError("manager has not been updated to the bundle image")
        nodes = get("nodes")
        for node in nodes["items"]:
            resources = node["status"].get("allocatable", {})
            if any("/" in k and any(x in k.lower() for x in ("gpu", "npu", "mlu", "ascend", "iluvatar"))
                   and str(v) != "0" for k, v in resources.items()):
                raise ValueError("this experiment requires zero locally advertised accelerators")
        write(self.folder / "nodes.json", nodes)
        for driver in self.templates:
            endpoint = get(f"acceleratorendpoint/{driver}-remote-server")
            if not observed_idle(endpoint):
                raise ValueError(f"{driver}: healthy, fresh session-free observation is required")
        system_pods = get("pods", SYSTEM)["items"]
        for pod in system_pods:
            if pod["metadata"].get("labels", {}).get("app.kubernetes.io/name") in {
                    "accelerator-control-plane", "corex-remote-server", "mlu-remote-server"}:
                self.verify_image_ids(pod)
        write(self.folder / "system-pods-before.json", {"items": system_pods})
        managers = [p for p in system_pods if p["metadata"].get("labels", {}).get("app.kubernetes.io/name") == "accelerator-control-plane"]
        if not managers:
            raise ValueError("no manager Pod is available")
        # Fail before creating tasks if the evidence transport is unavailable.
        (self.folder / "log-channel.txt").write_text(kubectl("logs", managers[0]["metadata"]["name"], "-n", SYSTEM, "-c", "manager", "--tail=1"))
        print("PASS preflight: new manager, imported images, no local accelerator resources, observed remote idle", flush=True)

    def verify_image_ids(self, pod):
        expected = json.loads((self.bundle / "bundle.json").read_text())["images"]
        statuses = pod.get("status", {}).get("containerStatuses", []) + pod.get("status", {}).get("initContainerStatuses", [])
        if not statuses:
            raise ValueError("Pod has no runtime image identities")
        for container in statuses:
            tag = container["image"].removeprefix("docker.io/library/")
            if tag not in expected or container.get("imageID") != expected[tag]["configDigest"]:
                raise ValueError("runtime config digest differs from delivery archive: " + tag)

    def create(self, driver, suffix, hold=0, exit_code=0, iterations=1):
        name = driver + "-" + suffix
        workload = copy.deepcopy(self.templates[driver])
        workload["metadata"] = {"name": name, "namespace": self.namespace}
        workload["spec"]["placement"].update(mode="Auto", order=["Local", "Remote"])
        workload["spec"]["placement"]["fallback"] = {"enabled": True, "maxAttempts": 2,
                                                      "startupTimeout": "2m", "backoff": "2s"}
        pod = workload["spec"]["podTemplate"]["spec"]
        pod["automountServiceAccountToken"] = False
        pod["activeDeadlineSeconds"] = 240
        worker = pod["containers"][0]
        app = "/opt/workload/driver_smoke_app /opt/workload/driver_kernel.cubin" if driver == "corex" else "/opt/workload/mlu-remote-custom-kernel-test"
        worker["command"] = ["/bin/sh", "-ec"]
        worker["args"] = [f'''for device in /dev/nvidia* /dev/iluvatar* /dev/cambricon* /dev/cndrv* /dev/davinci* /dev/ascend*; do
  if [ -e "$device" ]; then echo "FAIL local accelerator visible: $device"; exit 80; fi
done
echo LOCAL_ACCELERATOR_DEVICES=NONE
i=0
while [ "$i" -lt {iterations} ]; do
  {app}
  i=$((i + 1))
  if [ "$i" -lt {iterations} ]; then sleep 2; fi
done
echo KERNEL_FINISHED
sleep {hold}
exit {exit_code}
''']
        worker["securityContext"] = {"runAsNonRoot": True, "runAsUser": 65532,
                                     "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}}
        worker["resources"] = {"requests": {"cpu": "10m", "memory": "32Mi"}, "limits": {"memory": "128Mi"}}
        write(self.folder / (name + "-input.json"), workload)
        kubectl("create", "-f", "-", obj=workload)
        self.names.append(name)
        return name

    def workload(self, name):
        return get("acceleratorworkload/" + name, self.namespace)

    def terminal(self, name, expected="Succeeded"):
        def check():
            workload = self.workload(name)
            phase = workload.get("status", {}).get("phase")
            if phase in {"Succeeded", "Failed"}:
                if phase != expected:
                    raise ValueError(f"{name}: expected {expected}, got {phase}")
                return workload
        return wait(name + " terminal phase", check)

    def running(self, name):
        def check():
            workload = self.workload(name)
            status = workload.get("status", {})
            if status.get("phase") == "Failed":
                raise ValueError(name + " failed before Running")
            return workload if status.get("phase") == "Running" and status.get("fallback", {}).get("readyObserved") else None
        return wait(name + " observed Ready", check)

    def collect(self, name):
        folder = self.folder / name
        folder.mkdir(exist_ok=True)
        workload = self.workload(name)
        write(folder / "workload.json", workload)
        status = workload.get("status", {})
        if status.get("allocationRef"):
            write(folder / "lease.json", get("acceleratorlease/" + status["allocationRef"], self.namespace))
        if status.get("podRef", {}).get("name"):
            podname = status["podRef"]["name"]
            write(folder / "pod.json", get("pod/" + podname, self.namespace))
            (folder / "client.stdout").write_text(kubectl("logs", podname, "-n", self.namespace, "-c", "worker"))
        driver = name.split("-", 1)[0]
        write(folder / "endpoint.json", get("acceleratorendpoint/" + driver + "-remote-server"))
        return folder

    def verify(self, name):
        driver = name.split("-", 1)[0]
        def check():
            folder = self.collect(name)
            try:
                result = verifier.verify(folder, driver)
            except ValueError as error:
                write(folder / 'last-verification-rejection.json', {
                    'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(), 'error': str(error)})
                return None
            pod = json.loads((folder / "pod.json").read_text())
            self.verify_image_ids(pod)
            text = (folder / "client.stdout").read_text()
            if "LOCAL_ACCELERATOR_DEVICES=NONE" not in text:
                raise ValueError("missing proof that no local accelerator device was visible")
            spec = pod["spec"]
            if spec.get("hostPID") or spec.get("hostNetwork") or any("hostPath" in v for v in spec.get("volumes", [])):
                raise ValueError("client must not access host devices or SDK paths")
            if driver == "corex":
                worker = next(c for c in spec["containers"] if c["name"] == "worker")
                host = next(v["value"] for v in worker["env"] if v["name"] == "COREX_REMOTE_HOST")
                expected = json.loads((folder / "endpoint.json").read_text())["spec"]["address"].rsplit(":", 1)[0]
                if host != expected:
                    raise ValueError("Provider rewrote CoreX DNS")
                try:
                    ipaddress.ip_address(host)
                except ValueError:
                    pass
                else:
                    raise ValueError("CoreX must use stable hostname directly")
            (folder / "result.txt").write_text(result + "\nLOCAL_DEVICES=NONE; DNS unchanged where required\n")
            write(folder / 'verification.json', {'result': 'PASS', 'driver': driver,
                  'verifiedAt': dt.datetime.now(dt.timezone.utc).isoformat(),
                  'workloadUID': json.loads((folder / 'workload.json').read_text())['metadata']['uid'],
                  'podUID': pod['metadata']['uid'],
                  'leaseUID': json.loads((folder / 'lease.json').read_text())['metadata']['uid'],
                  'runtimeConfigDigestsMatched': True, 'localAcceleratorDevices': 'NONE'})
            return True
        wait(name + " numerical result and cleanup", check, 60)

    def idle(self, driver):
        return wait(driver + " session cleanup", lambda: observed_idle(get("acceleratorendpoint/" + driver + "-remote-server")), 90)

    def record(self, scenario, driver, action):
        print("RUN " + driver + " " + scenario, flush=True)
        action()
        self.results.append({"scenario": scenario, "driver": driver, "result": "PASS"})
        write(self.folder / "results.json", self.results)
        print("PASS " + driver + " " + scenario, flush=True)

    def smoke(self, driver):
        name = self.create(driver, "auto")
        self.terminal(name)
        self.verify(name)

    def competition(self, driver):
        names = [self.create(driver, "compete-" + str(i), hold=25) for i in range(2)]
        saw_waiter = False
        def check():
            nonlocal saw_waiter
            objects = [self.workload(n) for n in names]
            phases = [o.get("status", {}).get("phase") for o in objects]
            if "Failed" in phases:
                raise ValueError("competing task unexpectedly failed")
            active = [o for o in objects if o.get("status", {}).get("phase") in {"Starting", "Running"}]
            if len(active) > 1:
                raise ValueError("two competing tasks started on one exclusive Endpoint")
            if len(active) == 1 and any(o.get("status", {}).get("phase") == "Pending" and not o.get("status", {}).get("podRef") for o in objects):
                saw_waiter = True
            with (self.folder / (driver + "-competition.jsonl")).open("a") as trace:
                trace.write(json.dumps({"at": dt.datetime.now(dt.timezone.utc).isoformat(), "workloads": objects}) + "\n")
            return phases == ["Succeeded", "Succeeded"]
        wait(driver + " serialized competing tasks", check)
        if not saw_waiter:
            raise ValueError("did not observe a queued competitor")
        pods = []
        for name in names:
            self.verify(name)
            pods.append(json.loads((self.folder / name / "pod.json").read_text()))
        windows = sorted((p["status"]["containerStatuses"][0]["state"]["terminated"] for p in pods),
                         key=lambda w: w["startedAt"])
        if windows[1]["startedAt"] < windows[0]["finishedAt"]:
            raise ValueError("worker execution intervals overlap")

    def guard_disruption(self):
        for kind, terminal in (("acceleratorworkloads", {"Succeeded", "Failed"}),
                               ("acceleratorleases", {"Released", "Expired", "Failed"})):
            for obj in prepare.get(kind, "-A")["items"]:
                if obj["metadata"]["namespace"] != self.namespace and obj.get("status", {}).get("phase") not in terminal:
                    raise ValueError("refusing disruption: another namespace has active " + kind)
        for claim in prepare.get("leases", "-A", "-l", "accelerator.qcrs.io/claim-type=remote-endpoint")["items"]:
            if claim["metadata"].get("annotations", {}).get("accelerator.qcrs.io/lease-namespace") != self.namespace:
                raise ValueError("refusing disruption: another allocation holds an Endpoint Claim")

    def scale(self, driver, count):
        resource = ("statefulset/" if driver == "corex" else "deployment/") + driver + "-remote-server"
        kubectl("scale", resource, "-n", SYSTEM, "--replicas=" + str(count))
        if count:
            kubectl("rollout", "status", resource, "-n", SYSTEM, "--timeout=150s", timeout=180)

    def server_outage(self, driver, running=False):
        self.guard_disruption()
        resource = ("statefulset/" if driver == "corex" else "deployment/") + driver + "-remote-server"
        replicas = get(resource, SYSTEM)["spec"]["replicas"]
        if replicas != 1:
            raise ValueError("single-server test requires exactly one replica")
        self.idle(driver)
        name = self.create(driver, "ready-outage", iterations=300) if running else None
        original = self.running(name) if name else None
        try:
            self.guard_disruption()
            self.scale(driver, 0)
            if running:
                workload = self.terminal(name, "Failed")
                status = workload["status"]
                previous = original["status"]
                if (not status.get("fallback", {}).get("readyObserved") or status.get("fallback", {}).get("history")
                        or status.get("fallback", {}).get("failedAttempts", 0) != 0
                        or status.get("allocationRef") != previous["allocationRef"]
                        or status.get("podRef", {}).get("uid") != previous["podRef"]["uid"]):
                    raise ValueError("post-Ready outage caused a replay or lost the Ready boundary")
                folder = self.collect(name)
                text = (folder / "client.stdout").read_text()
                marker = "DRIVER_SMOKE_NUMERICAL=PASS bytes=256" if driver == "corex" else "MLU_SMOKE_NUMERICAL=PASS elements=1024"
                if marker not in text:
                    raise ValueError("outage task never executed a numerically verified kernel")
            else:
                wait(driver + " becomes unobserved", lambda: get("acceleratorendpoint/" + driver + "-remote-server").get("status", {}).get("activeSessions") is None, 90)
                name = self.create(driver, "queued-outage")
                for _ in range(8):
                    workload = self.workload(name)
                    status = workload.get("status", {})
                    if status.get("phase") not in {None, "Pending"} or status.get("podRef"):
                        raise ValueError("unavailable server admitted a new task")
                    with (self.folder / (driver + "-outage-pending.jsonl")).open("a") as trace:
                        trace.write(json.dumps(workload) + "\n")
                    time.sleep(2)
        finally:
            self.scale(driver, replicas)
        self.idle(driver) if running else None
        if not running:
            self.terminal(name)
            self.verify(name)
        else:
            workload = self.workload(name)
            if workload["status"]["phase"] != "Failed" or workload["status"]["allocationRef"] != original["status"]["allocationRef"]:
                raise ValueError("failed task changed after server recovery")
            lease = get("acceleratorlease/" + workload["status"]["allocationRef"], self.namespace)
            if lease["status"]["phase"] != "Released":
                raise ValueError("failed task lease was not released")
            write(self.folder / name / "endpoint-after-recovery.json", get("acceleratorendpoint/" + driver + "-remote-server"))

    def delete_running(self, driver):
        name = self.create(driver, "delete", hold=120)
        workload = self.running(name)
        self.collect(name)
        pod = workload["status"]["podRef"]["name"]
        lease = workload["status"]["allocationRef"]
        kubectl("delete", "acceleratorworkload/" + name, "-n", self.namespace, "--wait=false")
        wait(name + " finalizer cleanup", lambda: get("acceleratorworkload/" + name, self.namespace, True) is None, 120)
        wait(name + " Pod cleanup", lambda: get("pod/" + pod, self.namespace, True) is None, 90)
        self.idle(driver)
        remaining = get("acceleratorlease/" + lease, self.namespace, True)
        if remaining and remaining.get("status", {}).get("phase") != "Released":
            raise ValueError("deleted task left an active allocation")
        claims = prepare.get("leases", "-A", "-l", "accelerator.qcrs.io/claim-type=remote-endpoint")["items"]
        if any(c["metadata"].get("annotations", {}).get("accelerator.qcrs.io/lease-name") == lease for c in claims):
            raise ValueError("deleted task left an Endpoint Claim")
        write(self.folder / name / "deletion-result.json", {"workloadGone": True, "podGone": True,
              "leaseAbsentOrReleased": True, "claimGone": True, "endpoint": get("acceleratorendpoint/" + driver + "-remote-server")})

    def restart_controller(self):
        name = self.create("corex", "controller-restart", hold=60)
        original = self.running(name)
        self.guard_disruption()
        kubectl("rollout", "restart", "deployment/accelerator-manager", "-n", SYSTEM)
        kubectl("rollout", "status", "deployment/accelerator-manager", "-n", SYSTEM, "--timeout=150s", timeout=180)
        current = self.workload(name)
        for field in ("allocationRef", "podRef", "allocationAttempt"):
            if current["status"].get(field) != original["status"].get(field):
                raise ValueError("controller restart changed " + field)
        self.terminal(name)
        self.verify(name)
        write(self.folder / name / "before-restart.json", original)

    def recreate_corex(self):
        self.guard_disruption()
        self.idle("corex")
        before = get("pod/corex-remote-server-0", SYSTEM)
        address = get("acceleratorendpoint/corex-remote-server")["spec"]["address"]
        kubectl("delete", "pod/corex-remote-server-0", "-n", SYSTEM, "--wait=false")
        def check():
            pod = get("pod/corex-remote-server-0", SYSTEM, True)
            return pod if pod and pod["metadata"]["uid"] != before["metadata"]["uid"] and any(
                c["type"] == "Ready" and c["status"] == "True" for c in pod.get("status", {}).get("conditions", [])) else None
        after = wait("CoreX server recreated", check, 150)
        self.idle("corex")
        if after["status"]["podIP"] == before["status"]["podIP"]:
            raise ValueError("Pod IP was reused; this run cannot prove DNS address rotation")
        if address != get("acceleratorendpoint/corex-remote-server")["spec"]["address"]:
            raise ValueError("Endpoint address changed instead of remaining stable DNS")
        name = self.create("corex", "dns-recreated")
        self.terminal(name)
        self.verify(name)
        write(self.folder / name / "server-recreation.json", {"before": before, "after": after, "stableAddress": address})

    def corex_clusterip(self):
        self.guard_disruption()
        self.idle('corex')
        endpoint = get('acceleratorendpoint/corex-remote-server')
        original = endpoint['spec']['address']
        uid = endpoint['metadata']['uid']
        service = 'corex-nat-' + self.namespace.removeprefix('accelerator-validation-')
        address = service + '.' + SYSTEM + '.svc.cluster.local:50051'
        manifest = {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': service, 'namespace': SYSTEM,
                    'labels': {'accelerator.qcrs.io/validation': self.namespace}},
                    'spec': {'type': 'ClusterIP', 'selector': {'app.kubernetes.io/name': 'corex-remote-server'},
                    'ports': [{'name': 'remote', 'port': 50051, 'targetPort': 'remote'},
                              {'name': 'observation', 'port': 50052, 'targetPort': 'observation'}]}}
        write(self.folder / 'corex-clusterip-service-input.json', manifest)
        kubectl('create', '-f', '-', obj=manifest)
        changed = False
        name = None
        def set_address(expected, replacement):
            patch = [{'op': 'test', 'path': '/metadata/uid', 'value': uid},
                     {'op': 'test', 'path': '/spec/address', 'value': expected},
                     {'op': 'replace', 'path': '/spec/address', 'value': replacement}]
            kubectl('patch', 'acceleratorendpoint/corex-remote-server', '--type=json', '-p', json.dumps(patch))
        try:
            set_address(original, address)
            changed = True
            self.idle('corex')
            service_state = get('service/' + service, SYSTEM)
            cluster_ip = service_state['spec']['clusterIP']
            pod_ip = get('pod/corex-remote-server-0', SYSTEM)['status']['podIP']
            if not cluster_ip or cluster_ip == 'None' or cluster_ip == pod_ip:
                raise ValueError('test did not establish a separate Service virtual IP')
            # Reuse the same Endpoint and Claim domain; registering a second
            # Endpoint alias would incorrectly permit two physical owners.
            name = self.create('corex', 'clusterip')
            self.terminal(name)
            self.verify(name)
            text = (self.folder / name / 'client.stdout').read_text()
            host = address.rsplit(':', 1)[0]
            if 'host=' + host + ' ' not in text:
                raise ValueError('client trace did not confirm direct Service DNS usage')
            write(self.folder / name / 'service-routing.json', {'service': service_state,
                  'backendPodIP': pod_ip, 'serviceClusterIP': cluster_ip, 'endpointName': endpoint['metadata']['name'],
                  'temporaryAddress': address, 'originalAddress': original})
        finally:
            if name:
                current = get('acceleratorworkload/' + name, self.namespace, True)
                if current and current.get('status', {}).get('phase') not in {'Succeeded', 'Failed'}:
                    kubectl('delete', 'acceleratorworkload/' + name, '-n', self.namespace, '--wait=false')
                    wait('ClusterIP test workload cleanup', lambda: get('acceleratorworkload/' + name, self.namespace, True) is None, 120)
            if changed:
                # Atomic tests refuse to overwrite a concurrently replaced
                # Endpoint or an administrator's later address edit.
                set_address(address, original)
                self.idle('corex')
            kubectl('delete', 'service/' + service, '-n', SYSTEM, '--wait=true', '--timeout=30s')
        write(self.folder / 'corex-clusterip-restored.json', get('acceleratorendpoint/corex-remote-server'))

    def disruptions(self):
        for driver in ("corex", "mlu"):
            self.record("outage queues new task and recovery resumes it", driver, lambda: self.server_outage(driver))
            self.record("post-Ready server outage does not replay", driver, lambda: self.server_outage(driver, running=True))
            self.record("delete running task releases ownership", driver, lambda: self.delete_running(driver))
        self.record("controller restart retains allocation and Pod", "corex", self.restart_controller)
        self.record("stable DNS survives server Pod IP change", "corex", self.recreate_corex)
        self.record("ClusterIP DNS and Service NAT execute numerical kernel", "corex", self.corex_clusterip)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", required=True, type=Path)
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--preflight-only", action="store_true")
    parser.add_argument("--disruptions", action="store_true", help="also restart test services/controller; refuses other active tasks")
    parser.add_argument("--disruptions-only", action="store_true", help="run the disruptive matrix after a separate basic run")
    parser.add_argument("--clusterip-only", action="store_true", help="only test CoreX through an isolated ClusterIP Service and restore its Endpoint")
    args = parser.parse_args()
    suite = Suite(args.evidence, args.bundle)
    created = False
    try:
        suite.preflight()
        if args.preflight_only:
            return
        kubectl("create", "namespace", suite.namespace)
        created = True
        write(suite.folder / "namespace.json", get("namespace/" + suite.namespace))
        if args.clusterip_only:
            suite.record('ClusterIP DNS and Service NAT execute numerical kernel', 'corex', suite.corex_clusterip)
        elif not args.disruptions_only:
            for driver in ("corex", "mlu"):
                suite.record("Auto + numerical + cleanup", driver, lambda: suite.smoke(driver))
                suite.record("exclusive competition", driver, lambda: suite.competition(driver))
        if not args.clusterip_only and (args.disruptions or args.disruptions_only):
            suite.disruptions()
        write(suite.folder / "summary.json", {"result": "PASS", "namespace": suite.namespace,
              "disruptionsExecuted": args.disruptions or args.disruptions_only or args.clusterip_only, "scenarios": suite.results,
              "startedAt": suite.started_at, "finishedAt": dt.datetime.now(dt.timezone.utc).isoformat()})
    except (OSError, ValueError, RuntimeError, TimeoutError, subprocess.SubprocessError) as error:
        write(suite.folder / "summary.json", {"result": "FAIL", "error": str(error), "namespace": suite.namespace,
              "completed": suite.results, "startedAt": suite.started_at,
              "finishedAt": dt.datetime.now(dt.timezone.utc).isoformat()})
        if created:
            for name in suite.names:
                try:
                    suite.collect(name)
                except Exception as collection_error:
                    (suite.folder / (name + "-collection.error")).write_text(str(collection_error))
            # Only stop workloads created in this run's new namespace.
            for name in suite.names:
                workload = get("acceleratorworkload/" + name, suite.namespace, True)
                if workload and workload.get("status", {}).get("phase") not in {"Succeeded", "Failed"}:
                    kubectl("delete", "acceleratorworkload/" + name, "-n", suite.namespace,
                            "--ignore-not-found", "--wait=false")
        raise SystemExit("FAIL: " + str(error))
    finally:
        if created:
            write(suite.folder / "events.json", get("events", suite.namespace))
            write(suite.folder / "system-pods.json", get("pods", SYSTEM))


if __name__ == "__main__":
    main()
