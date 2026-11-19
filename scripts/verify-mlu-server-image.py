#!/usr/bin/env python3
"""Run the MLU server image on explicit devices and a client image without devices.
Uses only owned Docker resources; no host SDK mounts, privileged containers or K3s changes.
"""
import argparse
import json
from pathlib import Path
import re
import subprocess
import time
import urllib.request
import uuid


def run(args, **kwargs):
    return subprocess.check_output(args, text=True, timeout=30, **kwargs).strip()


def observation(ip):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open('http://' + ip + ':50052/v1/observation', timeout=3) as response:
        return json.load(response)


def idle(ip):
    deadline = time.monotonic() + 15
    last = None
    while time.monotonic() < deadline:
        try:
            last = observation(ip)
            if last['healthy'] and last['activeSessions'] == 0 and last['cleanupErrors'] == 0:
                return last
        except (OSError, ValueError):
            pass
        time.sleep(.25)
    raise RuntimeError('server image did not become healthy and session-free: ' + str(last))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--server-image', required=True)
    p.add_argument('--client-image', default='mlu-remote-client:arm64-observed-v1')
    p.add_argument('--workload-image', default='mlu-workload:arm64-observed-v1')
    p.add_argument('--evidence', required=True, type=Path)
    a = p.parse_args()
    a.evidence.mkdir(parents=True, exist_ok=False)
    prefix = 'acp-mlu-image-' + uuid.uuid4().hex[:12]
    network, server, client = prefix + '-net', prefix + '-server', prefix + '-client'
    volume = None
    server_created = network_created = False
    try:
        run(['docker', 'network', 'create', '--internal', network])
        network_created = True
        volume = run(['docker', 'volume', 'create'])
        run(['docker', 'run', '--rm', '--pull=never', '-v', volume + ':/runtime', a.client_image,
             '/bin/cp', '-a', '/opt/mlu-remote/lib/.', '/runtime/'])
        command = ['docker', 'run', '-d', '--rm', '--pull=never', '--name', server,
                   '--network', network, '--network-alias', 'mlu-validation']
        for device in ('cambricon_ctl', 'cambricon_dev0', 'cambricon_gdr', 'cambricon_ipcm0'):
            command += ['--device=/dev/' + device]
        command.append(a.server_image)
        run(command)
        server_created = True
        info = json.loads(run(['docker', 'inspect', server]))[0]
        ip = info['NetworkSettings']['Networks'][network]['IPAddress']
        before = idle(ip)
        client_command = ['docker', 'run', '--rm', '--pull=never', '--name', client, '--network', network,
                          '-v', volume + ':/opt/accelerator/mlu:ro', '-e', 'MLU_REMOTE_HOST=mlu-validation',
                          '-e', 'MLU_REMOTE_PORT=50051', '-e',
                          'LD_LIBRARY_PATH=/opt/accelerator/mlu:/opt/accelerator/mlu/shim:/opt/accelerator/mlu/driver-shim',
                          '-e', 'LD_PRELOAD=/opt/accelerator/mlu/shim/libcnrt.so:/opt/accelerator/mlu/driver-shim/libcndrv.so',
                          a.workload_image, '/bin/sh', '-ec',
                          'for d in /dev/cambricon* /dev/iluvatar* /dev/nvidia* /dev/davinci*; do '
                          'if [ -e "$d" ]; then echo "FAIL local device: $d"; exit 80; fi; done; '
                          'echo LOCAL_ACCELERATOR_DEVICES=NONE; exec /opt/workload/mlu-remote-custom-kernel-test']
        completed = subprocess.run(client_command, text=True, capture_output=True, timeout=45)
        (a.evidence/'client.stdout').write_text(completed.stdout)
        (a.evidence/'client.stderr').write_text(completed.stderr)
        text = completed.stdout + completed.stderr
        if (completed.returncode or 'MLU_SMOKE_NUMERICAL=PASS elements=1024' not in text
                or 'LOCAL_ACCELERATOR_DEVICES=NONE' not in text
                or not re.search(r'^PASS preset BANG vector_add module=\d+ kernel=\d+$', text, re.MULTILINE)):
            raise RuntimeError('real server image numerical test failed: exit=' + str(completed.returncode))
        after = idle(ip)
        (a.evidence/'result.json').write_text(json.dumps({'result': 'PASS', 'serverImage': a.server_image,
            'clientImage': a.workload_image, 'before': before, 'after': after,
            'numericalElements': 1024, 'clientExitCode': completed.returncode,
            'hostSDKMounted': False, 'privileged': False, 'samePhysicalHost': True}, indent=2)+'\n')
        print('PASS: self-contained MLU server image + no-device client, 1024 numerical elements, session cleanup', flush=True)
    finally:
        subprocess.run(['docker', 'rm', '-f', client], text=True, capture_output=True, timeout=20)
        if server_created:
            logs = subprocess.run(['docker', 'logs', server], text=True, capture_output=True, timeout=20)
            (a.evidence/'server.stdout').write_text(logs.stdout)
            (a.evidence/'server.stderr').write_text(logs.stderr)
            subprocess.run(['docker', 'rm', '-f', server], check=True, capture_output=True, timeout=20)
        if volume:
            subprocess.run(['docker', 'volume', 'rm', volume], check=True, capture_output=True, timeout=20)
        if network_created:
            subprocess.run(['docker', 'network', 'rm', network], check=True, capture_output=True, timeout=20)


if __name__ == '__main__':
    main()
