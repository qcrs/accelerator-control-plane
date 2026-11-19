import tempfile
from pathlib import Path
import unittest

from k3s_suite import ROOT, load, prepare


builder = load('remote_images', ROOT / 'scripts/build-remote-images.py')


class ImageDeliveryTest(unittest.TestCase):
    def test_missing_mlu_manifest_fails_before_partial_bundle_copy(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root/'modules').mkdir()
            (root/'out').mkdir()
            (root/'modules/vector_add.fatbin').write_bytes(b'kernel')
            with self.assertRaises(ValueError):
                builder.copy_mlu_operator_bundle(root, root/'out')
            self.assertEqual(list((root/'out').iterdir()), [])
            (root/'modules/vector_add.manifest').write_text('name=vector_add\n')
            builder.copy_mlu_operator_bundle(root, root/'out')
            self.assertEqual((root/'out/vector_add.fatbin').read_bytes(), b'kernel')
            self.assertEqual((root/'out/vector_add.manifest').read_text(), 'name=vector_add\n')

    def test_delivery_images_follow_independent_manifest_versions(self):
        objects = [{'kind': 'Deployment', 'spec': {'template': {'spec': {'containers': [{'image': 'mlu-server:v2'}]}}}},
                   {'kind': 'AcceleratorProvider', 'spec': {'clientRuntime': {'image': 'mlu-client:v1'}}},
                   {'kind': 'AcceleratorWorkload', 'spec': {'podTemplate': {'spec': {'containers': [{'image': 'mlu-workload:v1'}]}}}}]
        self.assertEqual(prepare.delivery_images('manager:v3', objects),
                         ['manager:v3', 'mlu-server:v2', 'mlu-client:v1', 'mlu-workload:v1'])

    def test_rollback_uses_the_captured_deployment_strategy(self):
        recreate = {'spec': {'strategy': {'type': 'Recreate'}}}
        patch = prepare.rollback_strategy_patch(recreate)
        self.assertEqual(patch['spec']['strategy'], {'type': 'Recreate', 'rollingUpdate': None})
        self.assertNotIn('rollingUpdate', recreate['spec']['strategy'])
        rolling = {'spec': {'strategy': {'type': 'RollingUpdate', 'rollingUpdate': {'maxSurge': 1, 'maxUnavailable': 0}}}}
        self.assertEqual(prepare.rollback_strategy_patch(rolling)['spec']['strategy'], rolling['spec']['strategy'])


if __name__ == '__main__':
    unittest.main()
