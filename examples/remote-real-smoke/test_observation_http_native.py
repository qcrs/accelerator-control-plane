"""Exercise the actual runtime HTTP parser over fragmented TCP requests, without hardware."""
import json
from pathlib import Path
import select
import socket
import subprocess
import tempfile
import unittest

from k3s_suite import ROOT


class ObservationHttpNativeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.folder = tempfile.TemporaryDirectory()
        cls.binary = Path(cls.folder.name) / 'server'
        subprocess.run(['cc', '-std=gnu11', '-O2', '-Wall', '-Wextra', '-Werror',
                        str(ROOT/'runtime/tests/observation_http_fixture.c'), '-o', str(cls.binary)], check=True)

    @classmethod
    def tearDownClass(cls):
        cls.folder.cleanup()

    def server(self):
        with socket.socket() as probe:
            probe.bind(('127.0.0.1', 0))
            port = probe.getsockname()[1]
        process = subprocess.Popen([str(self.binary), str(port)], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        def cleanup():
            if process.poll() is None:
                process.terminate()
            process.communicate(timeout=3)
        self.addCleanup(cleanup)
        self.assertTrue(select.select([process.stdout], [], [], 3)[0], 'fixture did not become ready')
        self.assertEqual(process.stdout.readline(), b'READY\n')
        connection = socket.create_connection(('127.0.0.1', port), timeout=2)
        self.addCleanup(connection.close)
        return connection

    def reply(self, connection):
        connection.settimeout(2)
        data = b''
        while True:
            chunk = connection.recv(4096)
            if not chunk:
                break
            data += chunk
        headers, body = data.split(b'\r\n\r\n', 1)
        self.assertTrue(headers.startswith(b'HTTP/1.1 200 OK'))
        self.assertEqual(json.loads(body)['activeSessions'], 0)

    def test_split_request_headers_are_consumed_before_reply_and_close(self):
        connection = self.server()
        connection.sendall(b'GET /v1/observation HTTP/1.1\r\n')
        connection.settimeout(.08)
        with self.assertRaises(socket.timeout):
            connection.recv(1)
        connection.sendall(b'Host: localhost\r\nConnection: close\r\n\r\n')
        self.reply(connection)

    def test_full_request_remains_compatible(self):
        connection = self.server()
        connection.sendall(b'GET /v1/observation HTTP/1.1\r\nHost: localhost\r\n\r\n')
        self.reply(connection)

    def test_unsupported_method_does_not_return_healthy(self):
        connection = self.server()
        connection.sendall(b'POST /v1/observation HTTP/1.1\r\nHost: localhost\r\n\r\n')
        self.assertEqual(connection.recv(1), b'')


if __name__ == '__main__':
    unittest.main()
