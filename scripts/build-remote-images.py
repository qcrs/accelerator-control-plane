#!/usr/bin/env python3
"""Build local runtime/workload images with explicit artifacts and library proof.
No import, deployment or push is performed. Tags are printed for review.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


def run(args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs)


def copy_mlu_operator_bundle(root, destination):
    files = [root/'modules'/('vector_add'+suffix) for suffix in ('.fatbin', '.manifest')]
    # The backend rejects a preset without its ABI manifest before native load.
    # Check the whole bundle before copying so incomplete builds fail early.
    for path in files:
        if not path.is_file() or path.stat().st_size == 0:
            raise ValueError('missing required MLU operator artifact: '+str(path))
    for path in files:
        shutil.copy2(path, destination/path.name)


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--corex-root', type=Path, required=True)
    p.add_argument('--mlu-root', type=Path, required=True)
    p.add_argument('--artifacts', type=Path, required=True)
    p.add_argument('--evidence', type=Path, required=True)
    p.add_argument('--tag', default='arm64-observed-v1')
    p.add_argument('--drivers', nargs='+', choices=('corex','mlu'), default=['corex','mlu'])
    p.add_argument('--kinds', nargs='+', choices=('server','client','workload'), default=['server','client','workload'])
    a = p.parse_args()
    a.evidence.mkdir(parents=True, exist_ok=True)
    platform = Path(__file__).resolve().parents[1]
    images = {}
    for driver, root in [('corex',a.corex_root), ('mlu',a.mlu_root)]:
        if driver not in a.drivers: continue
        libdir = a.artifacts/'corex-lib' if driver == 'corex' else root/'build'
        sdk = Path('/usr/local/corex-4.4.0' if driver == 'corex' else '/usr/local/neuware-3.5.2')
        for kind in a.kinds:
            with tempfile.TemporaryDirectory(prefix=f'{driver}-{kind}-') as tmp:
                ctx = Path(tmp)
                for sub in ('bin','lib','modules'):
                    (ctx/sub).mkdir()
                if kind == 'server':
                    binary = a.artifacts/(driver+'-server')
                    shutil.copy2(binary, ctx/'bin/server')
                    native_env = dict(os.environ, LD_LIBRARY_PATH=str(sdk/'lib64'))
                    native_env.pop('LD_PRELOAD', None)
                    deps = run(['ldd',str(binary)], env=native_env)
                    if 'not found' in deps:
                        raise RuntimeError(deps)
                    for soname, path in re.findall(r'^\s*(\S+) => (/\S+)', deps, re.MULTILINE):
                        shutil.copy2(path, ctx/'lib'/soname)
                    if driver == 'mlu':
                        copy_mlu_operator_bundle(root, ctx/'modules')
                        # cnModuleLoad dlopens the official fatbin parser. It
                        # is absent from the server's ELF NEEDED/ldd closure.
                        plugin = sdk/'lib64/libcnbin.so'
                        if not plugin.is_file():
                            raise ValueError('missing required MLU runtime plugin: '+str(plugin))
                        shutil.copy2(plugin, ctx/'lib/libcnbin.so')
                        plugin_deps = run(['ldd',str(plugin)], env=native_env)
                        if 'not found' in plugin_deps:
                            raise RuntimeError(plugin_deps)
                        for soname, path in re.findall(r'^\s*(\S+) => (/\S+)', plugin_deps, re.MULTILINE):
                            shutil.copy2(path, ctx/'lib'/soname)
                        (a.evidence/'mlu-libcnbin.ldd').write_text(plugin_deps)
                    (a.evidence/(driver+'-server.ldd')).write_text(deps)
                elif kind == 'client':
                    if driver == 'corex':
                        shutil.copytree(libdir,ctx/'lib',dirs_exist_ok=True,symlinks=True)
                    else:
                        for name in ('libremote_cnrt.so','libremote_cnnl.so'):
                            shutil.copy2(libdir/name,ctx/'lib'/name)
                        for sub in ('shim','driver-shim'):
                            shutil.copytree(libdir/sub,ctx/'lib'/sub,symlinks=True)
                else:
                    appname = 'driver_smoke_app' if driver=='corex' else 'mlu-remote-custom-kernel-test'
                    shutil.copy2(a.artifacts/(driver+'-smoke'),ctx/'bin'/appname)
                    if driver=='corex':
                        shutil.copy2(root/'build/driver_kernel.cubin',ctx/'bin/driver_kernel.cubin')
                manifest = {}
                for path in sorted(ctx.rglob('*')):
                    if not path.is_file(): continue
                    manifest[str(path.relative_to(ctx))] = hashlib.sha256(path.read_bytes()).hexdigest()
                (a.evidence/f'{driver}-{kind}.sha256.json').write_text(json.dumps(manifest,indent=2))
                for path in (ctx/'bin').iterdir():
                    (a.evidence/(driver+'-'+kind+'.file')).write_text(run(['file',str(path)]))
                    (a.evidence/(driver+'-'+kind+'.dynamic')).write_text(run(['readelf','-d',str(path)]))
                for library in (ctx/'lib').rglob('*.so*'):
                    if not library.is_file(): continue
                    label = str(library.relative_to(ctx)).replace('/', '_')
                    (a.evidence/(driver+'-'+kind+'-'+label+'.dynamic')).write_text(run(['readelf','-d',str(library)]))
                    (a.evidence/(driver+'-'+kind+'-'+label+'.file')).write_text(run(['file','-L',str(library)]))
                tag = driver+('-workload:' if kind=='workload' else '-remote-'+kind+':')+a.tag
                command=['docker','build','--platform','linux/arm64','--build-arg','DRIVER='+driver,'-f',str(platform/'runtime/images'/('Dockerfile.'+kind)),'-t',tag,str(ctx)]
                with (a.evidence/f'{driver}-{kind}.build.log').open('w') as output:
                    subprocess.run(command,stdout=output,stderr=subprocess.STDOUT,check=True)
                info=json.loads(run(['docker','image','inspect',tag]))[0]
                if info['Architecture']!='arm64': raise RuntimeError('wrong image architecture')
                images[tag]={'dockerImageID':info['Id'],'manifestDescriptor':info.get('Descriptor'),
                             'architecture':info['Architecture'],'repoDigests':info.get('RepoDigests',[])}
                # Verify native server dependency closure inside its image.
                if kind=='server':
                    proof=run(['docker','run','--rm','--entrypoint','/bin/sh',tag,'-c','ldd /opt/runtime/bin/server'])
                    (a.evidence/(driver+'-server.container-ldd')).write_text(proof)
                    if 'not found' in proof: raise RuntimeError(proof)
                    if driver=='mlu':
                        proof=run(['docker','run','--rm','--entrypoint','/bin/sh',tag,'-ec',
                                   'test -s /opt/runtime/modules/vector_add.fatbin; test -s /opt/runtime/modules/vector_add.manifest; test -s /opt/runtime/lib/libcnbin.so; sha256sum /opt/runtime/modules/vector_add.* /opt/runtime/lib/libcnbin.so'])
                        (a.evidence/'mlu-server.operator-bundle').write_text(proof)
                print(tag, info['Id'], flush=True)
    (a.evidence/'images.json').write_text(json.dumps(images,indent=2)+'\n')


if __name__=='__main__':
    main()
