#!/usr/bin/env python3
"""Single ARM machine: no-device client -> TCP -> real GPU/NPU server.
Never deploys to Kubernetes or changes an existing server. Only owned child
processes are stopped. Requires separately built validation binaries.
"""
import argparse
import contextlib
import json
import os
from pathlib import Path
import socket
import subprocess
import time
import urllib.request


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def snapshot(port):
    # Do not let HTTP_PROXY change the runtime being observed.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(f'http://127.0.0.1:{port}/v1/observation', timeout=3) as response:
        return json.load(response)


def wait_snapshot(port, sessions, timeout=8):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = snapshot(port)
            if last['healthy'] and last['activeSessions'] == sessions and last['cleanupErrors'] == 0:
                return last
        except (OSError, ValueError):
            pass
        time.sleep(.2)
    raise RuntimeError(f'observation did not converge: sessions={sessions}, last={last}')


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--corex-root', required=True, type=Path)
    p.add_argument('--mlu-root', required=True, type=Path)
    p.add_argument('--artifacts', required=True, type=Path)
    p.add_argument('--evidence', required=True, type=Path)
    p.add_argument('--image-tag', help='also run no-device Docker workload with runtime init-volume injection')
    args = p.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    platform = Path(__file__).resolve().parents[1]
    results = {}
    for driver, root in [('corex', args.corex_root), ('mlu', args.mlu_root)]:
        data_port, observation_port = free_port(), free_port()
        while data_port == observation_port:
            observation_port = free_port()
        out = args.evidence / driver
        out.mkdir(exist_ok=True)
        prefix = driver.upper() + '_REMOTE_'
        server_env = dict(os.environ)
        for name in ('LD_PRELOAD', 'LD_LIBRARY_PATH'):
            server_env.pop(name, None)
        server_env.update({prefix+'BIND': '127.0.0.1', prefix+'PORT': str(data_port), prefix+'OBSERVATION_PORT': str(observation_port)})
        server_env['LD_LIBRARY_PATH'] = '/usr/local/corex-4.4.0/lib64' if driver == 'corex' else '/usr/local/neuware-3.5.2/lib64'
        if driver == 'mlu':
            server_env['MLU_REMOTE_MODULE_DIR'] = str(root / 'modules')
        with (out/'server.stdout').open('w') as stdout, (out/'server.stderr').open('w') as stderr:
            server = subprocess.Popen([str(args.artifacts/(driver+'-server'))], env=server_env, stdout=stdout, stderr=stderr)
            try:
                before = wait_snapshot(observation_port, 0)
                (out/'observation-before.json').write_text(json.dumps(before, indent=2))
                # Real server admission/cleanup observation, including a client
                # that disconnects before HELLO. Probe requests are not sessions.
                with socket.create_connection(('127.0.0.1', data_port)):
                    active = wait_snapshot(observation_port, 1)
                    (out/'observation-active.json').write_text(json.dumps(active, indent=2))
                after_disconnect = wait_snapshot(observation_port, 0)
                (out/'observation-after-disconnect.json').write_text(json.dumps(after_disconnect, indent=2))
                client_env = dict(os.environ)
                client_env.pop('LD_PRELOAD', None)
                client_env.update({prefix+'HOST':'localhost', prefix+'PORT':str(data_port)})
                if driver == 'corex':
                    client_env['LD_LIBRARY_PATH'] = str(args.artifacts/'corex-lib')
                    client_env['COREX_SMOKE_TRACE'] = '1'
                    program = [str(args.artifacts/'corex-smoke'), str(root/'build/driver_kernel.cubin')]
                    sdk = '/usr/local/corex-4.4.0'
                else:
                    client_env['LD_LIBRARY_PATH'] = str(root/'build')
                    program = [str(args.artifacts/'mlu-smoke')]
                    sdk = str(Path('/usr/local/neuware-3.5.2').resolve())
                shell = '''
mount -t tmpfs tmpfs /dev
mount -t tmpfs tmpfs "$1"
shift
test ! -e /dev/iluvatar0
test ! -e /dev/cambricon_dev0
echo CLIENT_NO_GPU_NPU=PASS
echo CLIENT_HOSTNAME_DIRECT=localhost
exec "$@"
'''
                command = ['unshare','-Ur','-m','--propagation','private','bash','-eu','-c',shell,'bash',sdk]+program
                completed = subprocess.run(command, env=client_env, capture_output=True, timeout=30)
                (out/'client.stdout').write_bytes(completed.stdout)
                (out/'client.stderr').write_bytes(completed.stderr)
                (out/'exit-code').write_text(str(completed.returncode)+'\n')
                log = completed.stdout.decode() + completed.stderr.decode()
                marker = 'DRIVER_SMOKE_NUMERICAL=PASS bytes=256' if driver == 'corex' else 'MLU_SMOKE_NUMERICAL=PASS elements=1024'
                if completed.returncode or marker not in log or 'CLIENT_NO_GPU_NPU=PASS' not in log:
                    raise RuntimeError(f'{driver} numerical smoke failed: exit={completed.returncode}; see {out}')
                if args.image_tag:
                    volume = subprocess.check_output(['docker','volume','create'], text=True).strip()
                    try:
                        runtime_image = driver+'-remote-client:'+args.image_tag
                        workload_image = driver+'-workload:'+args.image_tag
                        mount = '/opt/accelerator/'+driver
                        subprocess.run(['docker','run','--rm','-v',volume+':/runtime',runtime_image,'/bin/cp','-a','/opt/'+driver+'-remote/lib/.','/runtime/'], check=True, capture_output=True)
                        injected_path = mount if driver=='corex' else ':'.join([mount,mount+'/shim',mount+'/driver-shim'])
                        docker_command = ['docker','run','--rm','--network','host','-v',volume+':'+mount+':ro','-e',prefix+'HOST=localhost','-e',prefix+'PORT='+str(data_port),'-e','LD_LIBRARY_PATH='+injected_path,'-e','COREX_SMOKE_TRACE=1']
                        if driver=='mlu':
                            docker_command += ['-e','LD_PRELOAD='+mount+'/shim/libcnrt.so:'+mount+'/driver-shim/libcndrv.so']
                        docker_command += [workload_image,'/bin/sh','-ec',
                            'test ! -e /dev/iluvatar0; test ! -e /dev/cambricon_dev0; echo CLIENT_NO_GPU_NPU=PASS; exec "$@"','sh']
                        docker_command += ['/opt/workload/driver_smoke_app','/opt/workload/driver_kernel.cubin'] if driver=='corex' else ['/opt/workload/mlu-remote-custom-kernel-test']
                        (out/'image-command.json').write_text(json.dumps(docker_command,indent=2))
                        image_run = subprocess.run(docker_command, capture_output=True, timeout=30)
                        (out/'image-client.stdout').write_bytes(image_run.stdout)
                        (out/'image-client.stderr').write_bytes(image_run.stderr)
                        (out/'image-exit-code').write_text(str(image_run.returncode)+'\n')
                        if image_run.returncode or marker not in (image_run.stdout+image_run.stderr).decode():
                            raise RuntimeError('image numerical smoke failed')
                    finally:
                        subprocess.run(['docker','volume','rm',volume],check=True,capture_output=True)
                after = wait_snapshot(observation_port, 0)
                (out/'observation-after.json').write_text(json.dumps(after, indent=2))
                # Actual Go provider reader -> actual native server contract.
                check_env = dict(os.environ, REMOTE_OBSERVATION_TEST_DRIVER=driver+'-remote', REMOTE_OBSERVATION_TEST_PORT=str(observation_port))
                check = subprocess.run(['go','test','./internal/provider/observation','-run','TestLiveRuntimeObservation','-count=1','-v'], cwd=platform, env=check_env, capture_output=True, timeout=60)
                (out/'provider-observation.log').write_bytes(check.stdout+check.stderr)
                if check.returncode:
                    raise RuntimeError('Go provider could not observe live runtime')
                if driver == 'corex':
                    import re
                    import signal
                    with socket.create_connection(('127.0.0.1', data_port)):
                        wait_snapshot(observation_port, 1)
                        worker = int(re.findall(r'SESSION_ACCEPTED pid=(\d+)', (out/'server.stdout').read_text())[-1])
                        stat = Path('/proc')/str(worker)/'stat'
                        if int(stat.read_text().split()[3]) != server.pid:
                            raise RuntimeError('fault injection target is not an owned worker')
                        os.kill(worker, signal.SIGKILL)
                        deadline = time.monotonic() + 5
                        while True:
                            fault = snapshot(observation_port)
                            if not fault['healthy'] and fault['cleanupErrors'] > 0:
                                break
                            if time.monotonic() > deadline:
                                raise RuntimeError('aborted worker was silently reported clean')
                            time.sleep(.2)
                        (out/'observation-cleanup-unconfirmed.json').write_text(json.dumps(fault,indent=2))
                results[driver] = {'cleanup_uncertainty': 'PASS' if driver=='corex' else 'covered-by-controller-and-parser-tests', 'image_numerical': 'PASS' if args.image_tag else 'not-run', 'numerical':'PASS','no_local_devices':'PASS','hostname_direct':'PASS','session_admission':'PASS','abrupt_disconnect_cleanup':'PASS','session_cleanup':'PASS','go_provider_observation':'PASS','exit_code':0}
            finally:
                server.terminate()
                try:
                    server.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait(timeout=5)
        # A stopped server must not be observed as healthy or idle.
        try:
            snapshot(observation_port)
        except OSError:
            results[driver]['server_down'] = 'PASS'
        else:
            raise RuntimeError('stopped runtime is still observable')
        print(driver + ': ' + json.dumps(results[driver]), flush=True)
    (args.evidence/'results.json').write_text(json.dumps(results, indent=2)+'\n')


if __name__ == '__main__':
    main()
