import unittest

from k3s_suite import ROOT, load


fix = load('k3s_proxy_fix', ROOT / 'scripts/fix-k3s-no-proxy.py')


class ProxyFixSafetyTest(unittest.TestCase):
    def test_bypass_preserves_existing_entries_and_adds_only_cluster_addresses(self):
        node = {'metadata': {'name': 'arm-test'}, 'status': {'addresses': [
            {'type': 'InternalIP', 'address': '192.168.31.244'}]}, 'spec': {'podCIDRs': ['10.42.0.0/24']}}
        value = fix.no_proxy_value(node, {'NO_PROXY': 'internal.example,127.0.0.1', 'no_proxy': 'other.example'})
        entries = value.split(',')
        for expected in ('internal.example', 'other.example', '127.0.0.1', '192.168.31.244', '10.42.0.0/24'):
            self.assertIn(expected, entries)
        self.assertNotIn('*', entries)
        self.assertEqual(entries.count('127.0.0.1'), 1)

    def test_quoted_or_multiline_environment_is_rejected(self):
        node = {'metadata': {'name': 'arm-test'}, 'status': {'addresses': []}}
        for value in ('example\nHTTP_PROXY=bad', "example'", 'example\\'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                fix.no_proxy_value(node, {'NO_PROXY': value})

    def test_unknown_or_live_ownership_prevents_service_restart(self):
        for phase in (None, 'Pending', 'Bound', 'Releasing'):
            with self.subTest(phase=phase), self.assertRaises(ValueError):
                fix.require_idle([], [{'status': {'phase': phase}}], [])
        with self.assertRaises(ValueError):
            fix.require_idle([{'status': {'phase': 'Running'}}], [], [])
        with self.assertRaises(ValueError):
            fix.require_idle([], [], [{'metadata': {'name': 'claim'}}])
        fix.require_idle([{'status': {'phase': 'Succeeded'}}], [{'status': {'phase': 'Released'}}], [])

    def test_install_does_not_replace_existing_files(self):
        import tempfile
        from pathlib import Path
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / 'existing'
            path.write_text('administrator settings')
            with self.assertRaises(FileExistsError):
                fix.new_file(path, 'replacement', 0o600)
            self.assertEqual(path.read_text(), 'administrator settings')


if __name__ == '__main__':
    unittest.main()
