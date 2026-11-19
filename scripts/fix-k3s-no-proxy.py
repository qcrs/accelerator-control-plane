#!/usr/bin/env python3
"""Prepare or apply an isolated K3s NO_PROXY override; never edits existing proxy credentials."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import subprocess


DROPIN = Path('/etc/systemd/system/k3s.service.d/90-accelerator-no-proxy.conf')
ENVFILE = Path('/etc/systemd/system/k3s-accelerator-no-proxy.env')
BUNDLE = Path('/tmp/accelerator-k3s-validation-20261119-v2')
MARKER = '# accelerator-control-plane: internal K3s traffic bypasses the external proxy\n'
DROPIN_TEXT = MARKER + '[Service]\nEnvironmentFile=' + str(ENVFILE) + '\n'


def output(args):
    return subprocess.check_output(args, text=True, timeout=40).strip()


def require_idle(workloads, leases, claims):
    for objects, terminal in ((workloads, {'Succeeded', 'Failed'}), (leases, {'Released', 'Failed', 'Expired'})):
        if any(obj.get('status', {}).get('phase') not in terminal for obj in objects):
            raise ValueError('refusing K3s restart while a Workload or allocation is active')
    if claims:
        raise ValueError('refusing K3s restart while any Endpoint Claim remains')


def inspect_cluster(root):
    cli = ['k3s', 'kubectl'] if root else ['kubectl']
    def get(*args):
        return json.loads(output(cli + ['--request-timeout=20s', 'get', *args, '-o', 'json']))
    nodes = get('nodes')['items']
    expected = json.loads((BUNDLE / 'bundle.json').read_text())['cluster']['nodeUIDs']
    if len(nodes) != 1 or sorted(n['metadata']['uid'] for n in nodes) != expected:
        raise ValueError('this repair is restricted to the prepared single-node validation cluster')
    require_idle(get('acceleratorworkloads', '-A')['items'], get('acceleratorleases', '-A')['items'],
                 get('leases', '-A', '-l', 'accelerator.qcrs.io/claim-type=remote-endpoint')['items'])
    return nodes[0]


def no_proxy_value(node, inherited):
    values = []
    for key in ('NO_PROXY', 'no_proxy'):
        values.extend(inherited.get(key, '').split(','))
    values.extend(['localhost', '127.0.0.1', '::1', node['metadata']['name'], '.svc', '.cluster.local', 'cluster.local'])
    for address in node['status']['addresses']:
        if address['type'] in {'InternalIP', 'ExternalIP'}:
            values.append(str(ipaddress.ip_address(address['address'])))
    values.extend(node.get('spec', {}).get('podCIDRs', []))
    if node.get('spec', {}).get('podCIDR'):
        values.append(node['spec']['podCIDR'])
    values = list(dict.fromkeys(v.strip() for v in values if v.strip()))
    if any(any(c in value for c in "\r\n\0'\"\\") for value in values):
        raise ValueError('NO_PROXY contains unsupported quoting or control characters')
    # K3s also appends its configured Service and Pod ranges at startup.
    return ','.join(values)


def inherited_environment(apply):
    if not apply:
        data = json.loads(Path('/tmp/accelerator-k3s-log-channel.json').read_text())
        return {k: v for k, v in data['proxyEnvironment'].items() if k.lower() == 'no_proxy'}
    pid = output(['systemctl', 'show', 'k3s', '--property=MainPID', '--value'])
    result = {}
    for item in (Path('/proc') / pid / 'environ').read_bytes().split(b'\0'):
        key, sep, value = item.decode(errors='replace').partition('=')
        if sep and key in {'NO_PROXY', 'no_proxy'}:
            result[key] = value
    return result


def new_file(path, text, mode):
    # Never replace an existing administrator-owned override or follow a link.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    with os.fdopen(fd, 'w') as stream:
        stream.write(text)
        stream.flush()
        os.fsync(stream.fileno())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', help='requires root; creates two new files and restarts K3s')
    args = parser.parse_args()
    if args.apply and os.geteuid() != 0:
        raise PermissionError('--apply must be run from an authorized sudo terminal')
    node = inspect_cluster(args.apply)
    value = no_proxy_value(node, inherited_environment(args.apply))
    env_text = MARKER + f"NO_PROXY='{value}'\nno_proxy='{value}'\n"
    plan = {'node': node['metadata']['name'], 'files': {str(DROPIN): DROPIN_TEXT, str(ENVFILE): env_text},
            'restarts': ['k3s'], 'originalEnvironmentFileModified': False,
            'rollbackCommands': ['sudo unlink ' + str(DROPIN), 'sudo unlink ' + str(ENVFILE),
                                 'sudo systemctl daemon-reload', 'sudo systemctl restart k3s']}
    print(json.dumps(plan, indent=2), flush=True)
    if not args.apply:
        print('PLAN ONLY: no files written and no service restarted.')
        return
    if DROPIN.exists() or DROPIN.is_symlink() or ENVFILE.exists() or ENVFILE.is_symlink():
        raise ValueError('override path already exists; inspect it rather than overwriting it')
    if DROPIN.parent.is_symlink():
        raise ValueError('refusing a symlinked systemd override directory')
    DROPIN.parent.mkdir(mode=0o755, exist_ok=True)
    created = []
    try:
        new_file(ENVFILE, env_text, 0o600)
        created.append(ENVFILE)
        new_file(DROPIN, DROPIN_TEXT, 0o644)
        created.append(DROPIN)
        inspect_cluster(True)  # Recheck immediately before the disruptive step.
        subprocess.run(['systemctl', 'daemon-reload'], check=True, timeout=30)
        subprocess.run(['systemctl', 'restart', 'k3s'], check=True, timeout=120)
    except Exception:
        for path in reversed(created):
            path.unlink()
        subprocess.run(['systemctl', 'daemon-reload'], check=False, timeout=30)
        # If a failed restart left the service down, start the original config.
        subprocess.run(['systemctl', 'start', 'k3s'], check=False, timeout=120)
        raise
    print('Override installed and K3s restarted. Verify node readiness and kubectl logs before accepting the repair.')


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        raise SystemExit('FAIL: ' + str(error))
