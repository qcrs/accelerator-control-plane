#!/usr/bin/env python3
"""Fail-closed acceptance of one captured remote smoke evidence set."""
import datetime
import json
from pathlib import Path
import re
import sys


def verify(folder, driver, now=None):
    root = Path(folder)
    def read(name):
        return json.loads((root / name).read_text())
    workload, lease, pod, endpoint = (read(name + '.json') for name in ('workload', 'lease', 'pod', 'endpoint'))
    ws = workload.get('status', {})
    if ws.get('phase') != 'Succeeded' or ws.get('selectedMode') != 'Remote':
        raise ValueError('Workload must have Succeeded in Remote mode')
    if ws.get('allocationRef') != lease.get('metadata', {}).get('name') or ws.get('podRef', {}).get('name') != pod.get('metadata', {}).get('name'):
        raise ValueError('Pod/Lease references do not match the captured Workload')
    targets = lease.get('spec', {}).get('targets', [])
    if len(targets) != 1 or not targets[0].get('exclusive') or targets[0].get('endpointName') != endpoint.get('metadata', {}).get('name'):
        raise ValueError('Lease must reference the captured exclusive Endpoint')
    if lease.get('status', {}).get('phase') != 'Released':
        raise ValueError('Lease is not Released')
    workers = [c for c in pod.get('status', {}).get('containerStatuses', []) if c.get('name') == 'worker']
    if len(workers) != 1 or workers[0].get('state', {}).get('terminated', {}).get('exitCode') != 0:
        raise ValueError('worker must terminate with exitCode=0')
    if pod.get('status', {}).get('phase') != 'Succeeded':
        raise ValueError('Pod did not succeed')
    text = (root / 'client.stdout').read_text()
    if driver == 'corex':
        required = [r'DRIVER_SMOKE_NUMERICAL=PASS bytes=256', r'DRIVER_SHIM_PASS ', r'kind=H2D .*result=PASS', r'kind=D2H .*result=PASS']
    elif driver == 'mlu':
        # client_custom_kernel prints this only after H2D, kernel launch,
        # queue sync, D2H and the 1024-element CPU reference comparison.
        required = [r'^MLU_SMOKE_NUMERICAL=PASS elements=1024$', r'^PASS preset BANG vector_add module=\d+ kernel=\d+$']
    else:
        raise ValueError('unknown driver')
    if any(not re.search(pattern, text, re.MULTILINE) for pattern in required):
        raise ValueError('missing explicit kernel/numerical verification markers')
    if re.search(r'NUMERICAL=FAIL|kernel mismatch|^FAIL\b', text, re.MULTILINE):
        raise ValueError('numerical failure reported')
    status = endpoint.get('status', {})
    if status.get('health') != 'Healthy' or status.get('confidence') != 'Observed' or status.get('observationSource') != 'remote-probe':
        raise ValueError('remote health is not observed')
    if status.get('activeSessions') != 0 or status.get('reserved', 0) != 0:
        raise ValueError('remote session/lease cleanup is not confirmed')
    stamp = datetime.datetime.fromisoformat(status.get('observedAt', '').replace('Z', '+00:00'))
    age = ((now or datetime.datetime.now(datetime.timezone.utc)) - stamp).total_seconds()
    if not 0 <= age <= 60:
        raise ValueError('cleanup observation is stale')
    ready = [c for c in status.get('conditions', []) if c.get('type') == 'Ready']
    if len(ready) != 1 or ready[0].get('status') != 'True' or ready[0].get('observedGeneration') != endpoint.get('metadata', {}).get('generation'):
        raise ValueError('Endpoint lacks a current Ready condition')
    return 'PASS: numerical result, exit code, Released Lease and observed session cleanup'


if __name__ == '__main__':
    try:
        print(verify(sys.argv[1], sys.argv[2]))
    except (ValueError, KeyError, OSError, IndexError) as error:
        print('FAIL: ' + str(error), file=sys.stderr)
        sys.exit(1)
