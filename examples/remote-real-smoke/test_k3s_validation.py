"""Regression checks for the acceptance harness's mutation and digest boundaries."""
import datetime as dt
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from k3s_suite import Suite, observed_idle, prepare


class ValidationSafetyTest(unittest.TestCase):
    def source(self, workload_phase="Succeeded", lease_phase="Released", claims=None):
        def get(kind, *args):
            if kind == "leases":
                return {"items": claims or []}
            phase = workload_phase if kind == "acceleratorworkloads" else lease_phase
            return {"items": [{"metadata": {"name": "task", "namespace": "business"},
                               "status": {} if phase is None else {"phase": phase}}]}
        return get

    def test_idle_guard_rejects_unknown_pending_and_releasing_ownership(self):
        for phase in (None, "Pending", "Reserved", "Bound", "Releasing"):
            with self.subTest(phase=phase), patch.object(prepare, "get", side_effect=self.source(lease_phase=phase)):
                with self.assertRaises(ValueError):
                    prepare.require_idle()

    def test_idle_guard_rejects_workload_without_a_lease(self):
        with patch.object(prepare, "get", side_effect=self.source(workload_phase="Pending")):
            with self.assertRaises(ValueError):
                prepare.require_idle()

    def test_leftover_claim_blocks_deployment_even_when_ledger_is_terminal(self):
        with patch.object(prepare, "get", side_effect=self.source(claims=[{"metadata": {"name": "held"}}])):
            with self.assertRaises(ValueError):
                prepare.require_idle()
        with patch.object(prepare, "get", side_effect=self.source()):
            prepare.require_idle()

    def test_context_change_blocks_bundle_before_any_cluster_action(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "bundle.json").write_text(json.dumps({"cluster": {"context": "expected", "nodeUIDs": ["node"]}}))
            with patch.object(prepare, "identity", return_value={"context": "other", "nodeUIDs": ["node"]}), patch.object(prepare, "get") as get:
                with self.assertRaises(ValueError):
                    prepare.check_bundle(root)
                get.assert_not_called()

    def test_disruption_refuses_foreign_active_tasks(self):
        suite = object.__new__(Suite)
        suite.namespace = "accelerator-validation-unit"
        with patch.object(prepare, "get", side_effect=self.source(workload_phase="Running")), patch("k3s_suite.kubectl") as kubectl:
            with self.assertRaises(ValueError):
                suite.guard_disruption()
            kubectl.assert_not_called()

    def test_empty_and_expired_health_never_admit_tasks(self):
        self.assertFalse(observed_idle({"metadata": {"generation": 1}}))
        now = dt.datetime.now(dt.timezone.utc)
        endpoint = {"metadata": {"generation": 1}, "status": {"health": "Healthy", "confidence": "Observed",
                    "observationSource": "remote-probe", "activeSessions": 0, "reserved": 0,
                    "observedAt": now.isoformat(), "conditions": [{"type": "Ready", "status": "True", "observedGeneration": 1}]}}
        self.assertTrue(observed_idle(endpoint))
        endpoint["status"]["observedAt"] = (now - dt.timedelta(minutes=2)).isoformat()
        self.assertFalse(observed_idle(endpoint))

    def test_multi_document_kubectl_json_is_decoded_without_dropping_objects(self):
        text = '{"kind":"Service"}\n{"kind":"List","items":[{"kind":"Deployment"}]}\n'
        self.assertEqual([o["kind"] for o in prepare.decode_objects(text)], ["Service", "Deployment"])

    def archive(self, path, corrupt=False):
        config = json.dumps({"architecture": "arm64", "rootfs": {"type": "layers", "diff_ids": []}}).encode()
        config_hash = hashlib.sha256(config).hexdigest()
        manifest = json.dumps({"schemaVersion": 2, "config": {"digest": "sha256:" + ("0" * 64 if corrupt else config_hash)}}).encode()
        manifest_hash = hashlib.sha256(manifest).hexdigest()
        files = {"blobs/sha256/" + config_hash: config, "blobs/sha256/" + manifest_hash: manifest,
                 "manifest.json": json.dumps([{"Config": "blobs/sha256/" + config_hash, "RepoTags": ["example:arm64"]}]).encode(),
                 "index.json": json.dumps({"manifests": [{"digest": "sha256:" + manifest_hash,
                        "annotations": {"io.containerd.image.name": "docker.io/library/example:arm64"}}]}).encode()}
        with tarfile.open(path, "w") as archive:
            for name, data in files.items():
                item = tarfile.TarInfo(name)
                item.size = len(data)
                archive.addfile(item, io.BytesIO(data))
        return "sha256:" + config_hash, "sha256:" + manifest_hash

    def test_oci_manifest_and_config_digests_are_not_confused(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "images.tar"
            config, manifest = self.archive(path)
            result = prepare.archive_image_metadata(path)["example:arm64"]
            self.assertNotEqual(config, manifest)
            self.assertEqual(result["configDigest"], config)
            self.assertEqual(result["manifestDigest"], manifest)

    def test_runtime_image_check_includes_init_container_and_rejects_wrong_digest_kind(self):
        with tempfile.TemporaryDirectory() as folder:
            suite = object.__new__(Suite)
            suite.bundle = Path(folder)
            (suite.bundle / "bundle.json").write_text(json.dumps({"images": {
                "worker:v1": {"configDigest": "sha256:worker-config", "manifestDigest": "sha256:worker-manifest"},
                "client:v1": {"configDigest": "sha256:client-config"}}}))
            pod = {"status": {"containerStatuses": [{"image": "docker.io/library/worker:v1", "imageID": "sha256:worker-config"}],
                              "initContainerStatuses": [{"image": "client:v1", "imageID": "sha256:client-config"}]}}
            suite.verify_image_ids(pod)
            pod["status"]["initContainerStatuses"][0]["imageID"] = "sha256:wrong"
            with self.assertRaises(ValueError):
                suite.verify_image_ids(pod)
            pod["status"]["initContainerStatuses"][0]["imageID"] = "sha256:client-config"
            pod["status"]["containerStatuses"][0]["imageID"] = "sha256:worker-manifest"
            with self.assertRaises(ValueError):
                suite.verify_image_ids(pod)

    def test_mismatched_archive_config_is_rejected(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "images.tar"
            self.archive(path, corrupt=True)
            with self.assertRaises(ValueError):
                prepare.archive_image_metadata(path)


if __name__ == "__main__":
    unittest.main()
