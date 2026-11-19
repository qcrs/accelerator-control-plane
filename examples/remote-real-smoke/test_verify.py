import datetime
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('smoke_verify', Path(__file__).with_name('verify.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class SmokeVerdictTest(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory()
        self.addCleanup(self.folder.cleanup)
        self.root = Path(self.folder.name)
        self.now = datetime.datetime.now(datetime.timezone.utc)
        self.objects = {
            'workload': {'status': {'phase': 'Succeeded', 'selectedMode': 'Remote', 'allocationRef': 'lease', 'podRef': {'name': 'pod'}}},
            'lease': {'metadata': {'name': 'lease'}, 'spec': {'targets': [{'exclusive': True, 'endpointName': 'endpoint'}]}, 'status': {'phase': 'Released'}},
            'pod': {'metadata': {'name': 'pod'}, 'status': {'phase': 'Succeeded', 'containerStatuses': [{'name': 'worker', 'state': {'terminated': {'exitCode': 0}}}]}},
            'endpoint': {'metadata': {'name': 'endpoint', 'generation': 1}, 'status': {'health': 'Healthy', 'confidence': 'Observed', 'observationSource': 'remote-probe', 'activeSessions': 0, 'reserved': 0, 'observedAt': self.now.isoformat(), 'conditions': [{'type': 'Ready', 'status': 'True', 'observedGeneration': 1}]}},
        }
        self.log = 'DRIVER_SMOKE_NUMERICAL=PASS bytes=256\nDRIVER_SHIM_PASS version=10020\nkind=H2D result=PASS\nkind=D2H result=PASS\n'

    def run_verdict(self):
        for name, value in self.objects.items():
            (self.root / (name + '.json')).write_text(json.dumps(value))
        (self.root / 'client.stdout').write_text(self.log)
        return module.verify(self.root, 'corex', self.now)

    def test_complete_evidence_passes(self):
        self.assertTrue(self.run_verdict().startswith('PASS'))

    def test_failed_and_timed_out_workloads_never_pass(self):
        for phase in ('Failed', 'Pending', 'Running', ''):
            with self.subTest(phase=phase):
                self.objects['workload']['status']['phase'] = phase
                with self.assertRaises(ValueError): self.run_verdict()

    def test_exit_zero_without_numerical_log_fails(self):
        self.log = 'container started\n'
        with self.assertRaises(ValueError): self.run_verdict()

    def test_missing_or_wrong_pod_fails(self):
        self.objects['workload']['status']['podRef'] = {}
        with self.assertRaises(ValueError): self.run_verdict()

    def test_nonzero_exit_fails(self):
        self.objects['pod']['status']['containerStatuses'][0]['state']['terminated']['exitCode'] = 1
        with self.assertRaises(ValueError): self.run_verdict()

    def test_lease_or_session_still_active_fails(self):
        self.objects['lease']['status']['phase'] = 'Releasing'
        with self.assertRaises(ValueError): self.run_verdict()
        self.objects['lease']['status']['phase'] = 'Released'
        self.objects['endpoint']['status']['activeSessions'] = 1
        with self.assertRaises(ValueError): self.run_verdict()

    def test_missing_and_stale_observations_fail(self):
        del self.objects['endpoint']['status']['activeSessions']
        with self.assertRaises(ValueError): self.run_verdict()
        self.objects['endpoint']['status']['activeSessions'] = 0
        self.objects['endpoint']['status']['observedAt'] = (self.now - datetime.timedelta(minutes=2)).isoformat()
        with self.assertRaises(ValueError): self.run_verdict()

    def test_mlu_memory_only_is_not_kernel_evidence(self):
        self.run_verdict()
        (self.root / 'client.stdout').write_text('PASS memory H2D/D2D/D2H\n')
        with self.assertRaises(ValueError): module.verify(self.root, 'mlu', self.now)
        (self.root / 'client.stdout').write_text('MLU_SMOKE_NUMERICAL=PASS elements=1024\nPASS preset BANG vector_add module=1 kernel=2\n')
        self.assertTrue(module.verify(self.root, 'mlu', self.now).startswith('PASS'))


if __name__ == '__main__':
    unittest.main()
