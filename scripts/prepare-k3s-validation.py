#!/usr/bin/env python3
"""Prepare a local K3s delivery bundle without changing the cluster or using sudo."""
import argparse
import copy
import hashlib
import json
from pathlib import Path
import shlex
import subprocess
import tarfile


ROOT = Path(__file__).resolve().parents[1]
SYSTEM = "accelerator-system"
MANAGER = "accelerator-control-plane:arm64-logic-20261119-v3"


def run(args):
    return subprocess.check_output(args, text=True, timeout=60)


def decode_objects(text):
    # kubectl can emit concatenated JSON objects for multiple input documents.
    decoder = json.JSONDecoder()
    objects = []
    while text.strip():
        obj, end = decoder.raw_decode(text.lstrip())
        objects.extend(obj.get("items", [obj]))
        text = text.lstrip()[end:]
    return objects


def get(*args):
    text = run(["kubectl", "get", *args, "-o", "json"])
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        return {"apiVersion": "v1", "kind": "List", "items": decode_objects(text)}


def require_idle():
    busy = []
    for kind, terminal in (("acceleratorworkloads", {"Succeeded", "Failed"}),
                           ("acceleratorleases", {"Released", "Failed", "Expired"})):
        for obj in get(kind, "-A")["items"]:
            if obj.get("status", {}).get("phase") not in terminal:
                busy.append(f'{kind}/{obj["metadata"]["namespace"]}/{obj["metadata"]["name"]}')
    claims = get("leases", "-A", "-l", "accelerator.qcrs.io/claim-type=remote-endpoint")["items"]
    if busy or claims:
        raise ValueError(f"cluster is not idle: objects={busy}; endpoint claims={len(claims)}")


def identity():
    nodes = get("nodes")["items"]
    return {"context": run(["kubectl", "config", "current-context"]).strip(),
            "nodeUIDs": sorted(n["metadata"]["uid"] for n in nodes)}


def check_bundle(bundle):
    info = json.loads((bundle / "bundle.json").read_text())
    if identity() != info["cluster"]:
        raise ValueError("context or node identity changed; prepare a fresh bundle")
    require_idle()
    return info


def check_images(bundle):
    info = check_bundle(bundle)
    missing = {}
    for node in get("nodes")["items"]:
        if node["metadata"].get("labels", {}).get("kubernetes.io/arch") != "arm64":
            raise ValueError("this bundle requires an ARM64-only validation cluster")
        names = {name for image in node["status"].get("images", []) for name in image.get("names") or []}
        absent = [tag for tag in info["images"] if "docker.io/library/" + tag not in names and tag not in names]
        if absent:
            missing[node["metadata"]["name"]] = absent
    if missing:
        raise ValueError("new images are not reported by kubelet; import the archive first: " + json.dumps(missing))
    print("PASS: same cluster, no active tasks/claims, and all image tags reported by kubelet")
    print("Image tags are a preflight check; runtime image digests still need checking after rollout.")


def write_json(path, obj):
    path.write_text(json.dumps(obj, indent=2) + "\n")


def archive_image_metadata(archive):
    result = {}
    with tarfile.open(archive) as stream:
        entries = json.load(stream.extractfile("manifest.json"))
        descriptors = {}
        if "index.json" in stream.getnames():
            for descriptor in json.load(stream.extractfile("index.json"))["manifests"]:
                name = descriptor.get("annotations", {}).get("io.containerd.image.name", "")
                descriptors[name.removeprefix("docker.io/library/")] = descriptor
        for entry in entries:
            data = stream.extractfile(entry["Config"]).read()
            config = json.loads(data)
            digest = "sha256:" + hashlib.sha256(data).hexdigest()
            for tag in entry.get("RepoTags") or []:
                item = {"configDigest": digest, "architecture": config["architecture"]}
                if tag in descriptors:
                    descriptor = descriptors[tag]
                    blob = stream.extractfile("blobs/" + descriptor["digest"].replace(":", "/")).read()
                    manifest_digest = "sha256:" + hashlib.sha256(blob).hexdigest()
                    if manifest_digest != descriptor["digest"] or json.loads(blob)["config"]["digest"] != digest:
                        raise ValueError("archive manifest/config digest mismatch: " + tag)
                    item["manifestDigest"] = manifest_digest
                result[tag] = item
    return result


def clean(obj):
    obj = copy.deepcopy(obj)
    meta = obj["metadata"]
    obj["metadata"] = {key: meta[key] for key in ("name", "namespace", "labels", "annotations") if key in meta}
    obj["metadata"].get("annotations", {}).pop("kubectl.kubernetes.io/last-applied-configuration", None)
    obj.pop("status", None)
    return obj


def sample(path):
    return decode_objects(run(["kubectl", "create", "--dry-run=client", "--validate=false", "-f", str(path), "-o", "json"]))


def delivery_images(manager, objects):
    # Manifests are the authority; server/client tags can evolve independently.
    tags = [manager]
    for obj in objects:
        if obj['kind'] in {'Deployment', 'StatefulSet'}:
            spec = obj['spec']['template']['spec']
        elif obj['kind'] == 'AcceleratorWorkload':
            spec = obj['spec']['podTemplate']['spec']
        elif obj['kind'] == 'AcceleratorProvider':
            tags.append(obj['spec']['clientRuntime']['image'])
            continue
        else:
            continue
        tags.extend(c['image'] for c in spec.get('initContainers', []) + spec['containers'])
    return list(dict.fromkeys(tags))


def rollback_strategy_patch(deployment):
    strategy = copy.deepcopy(deployment['spec']['strategy'])
    if strategy['type'] == 'Recreate':
        strategy['rollingUpdate'] = None
    return {'spec': {'strategy': strategy}}


def prepare(output, manager):
    require_idle()
    output.mkdir(parents=True, exist_ok=False)
    info = {"cluster": identity(), "images": {}, "clusterModified": False}
    servers, configuration = [], []
    for driver in ('corex','mlu'):
        servers.extend(sample(ROOT / f'config/samples/{driver}-remote-server.yaml'))
        configuration.extend(sample(ROOT / f'config/samples/{driver}-remote.yaml'))
    tags = delivery_images(manager, servers + configuration)
    for tag in tags:
        image = json.loads(run(["docker", "image", "inspect", tag]))[0]
        if image["Architecture"] != "arm64" or image["Os"] != "linux":
            raise ValueError("wrong image architecture: " + tag)
        # Docker's image ID can identify a manifest in the containerd image
        # store. Derive config digests from archive bytes, never from this ID.
        info["images"][tag] = {"dockerImageID": image["Id"], "architecture": image["Architecture"]}
    resources = get("deployment/accelerator-manager", "deployment/mlu-remote-server",
                    "statefulset/corex-remote-server", "service/corex-remote-server",
                    "service/mlu-remote-server", "-n", SYSTEM)["items"]
    configs = [o for o in get("acceleratorproviders,acceleratorpools,acceleratorendpoints")["items"]
               if o["metadata"]["name"] in {"corex-remote", "mlu-remote", "corex-remote-server", "mlu-remote-server"}]
    write_json(output / "before.json", {"apiVersion": "v1", "kind": "List", "items": resources + configs})
    write_json(output / "rollback.json", {"apiVersion": "v1", "kind": "List", "items": [clean(o) for o in resources + configs]})
    (output / "crds.yaml").write_text(run(["kubectl", "kustomize", str(ROOT / "config/crd")]))
    new_configs = [o for o in configuration if o['kind'] != 'AcceleratorWorkload']
    for obj in servers:
        if obj["kind"] in {"Deployment", "StatefulSet"}:
            for c in obj["spec"]["template"]["spec"]["containers"]:
                c["imagePullPolicy"] = "Never"
    write_json(output / "servers.json", {"apiVersion": "v1", "kind": "List", "items": servers})
    write_json(output / "configuration.json", {"apiVersion": "v1", "kind": "List", "items": new_configs})
    current = next(o for o in resources if o["kind"] == "Deployment" and o["metadata"]["name"] == "accelerator-manager")
    current = clean(current)
    container = next(c for c in current["spec"]["template"]["spec"]["containers"] if c["name"] == "manager")
    container.update(image=manager, imagePullPolicy="Never")
    write_json(output / "manager.json", current)
    qroot, qout = shlex.quote(str(ROOT)), shlex.quote(str(output.resolve()))
    check = f"python3 {qroot}/scripts/prepare-k3s-validation.py"
    previous_mlu = next(o for o in resources if o['kind'] == 'Deployment' and o['metadata']['name'] == 'mlu-remote-server')
    restore_strategy = shlex.quote(json.dumps(rollback_strategy_patch(previous_mlu)))
    (output / "deploy.sh").write_text(f'''#!/usr/bin/env bash
set -euo pipefail
{check} --check-images {qout}
kubectl apply --server-side -f {qout}/crds.yaml
kubectl -n {SYSTEM} patch deployment mlu-remote-server --type=merge -p '{{"spec":{{"strategy":{{"type":"Recreate","rollingUpdate":null}}}}}}'
kubectl apply -f {qout}/servers.json
kubectl -n {SYSTEM} rollout status statefulset/corex-remote-server --timeout=180s
kubectl -n {SYSTEM} rollout status deployment/mlu-remote-server --timeout=180s
kubectl apply -f {qout}/configuration.json
kubectl apply -f {qout}/manager.json
kubectl -n {SYSTEM} rollout status deployment/accelerator-manager --timeout=180s
kubectl wait acceleratorendpoint/corex-remote-server acceleratorendpoint/mlu-remote-server --for=condition=Ready --timeout=90s
''')
    (output / "rollback.sh").write_text(f'''#!/usr/bin/env bash
set -euo pipefail
{check} --check-bundle {qout}
kubectl -n {SYSTEM} patch deployment mlu-remote-server --type=merge -p {restore_strategy}
kubectl apply -f {qout}/rollback.json
kubectl -n {SYSTEM} rollout status statefulset/corex-remote-server --timeout=180s
kubectl -n {SYSTEM} rollout status deployment/mlu-remote-server --timeout=180s
kubectl -n {SYSTEM} rollout status deployment/accelerator-manager --timeout=180s
# Keep the additive CRD schema; do not delete CRDs or existing task history.
''')
    print(f"Saving {len(tags)} local images; cluster remains unchanged.", flush=True)
    archive = output / "images.tar"
    subprocess.run(["docker", "image", "save", "-o", str(archive), *tags], check=True, timeout=600)
    for tag, metadata in archive_image_metadata(archive).items():
        info["images"][tag].update(metadata)
    hasher = hashlib.sha256()
    with archive.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(block)
    digest = hasher.hexdigest()
    info.update(archiveSHA256=digest, archiveBytes=archive.stat().st_size)
    write_json(output / "bundle.json", info)
    (output / "images.tar.sha256").write_text(digest + "  images.tar\n")
    (output / "README.txt").write_text(f'''This bundle has not changed the cluster. No credentials or Secrets are included.
First review bundle.json, servers.json, configuration.json, manager.json and rollback.json.
Import on the K3s node using an authorized host terminal:
  cd {qout}
  sha256sum -c images.tar.sha256
  sudo k3s ctr --namespace k8s.io images import {qout}/images.tar
Then the unprivileged deployment entry is:
  bash {qout}/deploy.sh
The idle guard refuses active Workloads, unreleased Leases and leftover Endpoint Claims.
Server manifests retain the existing hardware test device permissions; no host runtime socket is mounted.
Rollback restores captured configuration and images, retaining the additive CRD schema:
  bash {qout}/rollback.sh
''')
    print("Prepared " + str(output.resolve()), flush=True)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    mode = p.add_mutually_exclusive_group(required=True)
    mode.add_argument("--output", type=Path)
    mode.add_argument("--check-idle", action="store_true")
    mode.add_argument("--check-images", type=Path)
    mode.add_argument("--check-bundle", type=Path)
    p.add_argument("--manager-image", default=MANAGER)
    a = p.parse_args()
    if a.check_idle:
        require_idle()
        print("PASS: no active Workloads, unreleased Leases or Endpoint Claims")
    elif a.check_images:
        check_images(a.check_images)
    elif a.check_bundle:
        check_bundle(a.check_bundle)
        print("PASS: same cluster and no active tasks/claims")
    else:
        prepare(a.output, a.manager_image)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        raise SystemExit("FAIL: " + str(error))
