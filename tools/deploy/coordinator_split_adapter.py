"""Opt-in adapter for a newly rendered, independently reviewed coordinator.

Not installed. The renderer retains original call/preimage/window functions.
Existing operation directories must never be converted/adopted with this code.
"""
import hashlib
import json
import os
import runpy
import subprocess
import sys
from pathlib import Path
import fcntl
import signal

import prepared_release
from prepared_image_content import verify_content



def read_blob(args, fds, timeout=30):
    """Existing coordinator process-group discipline, with binary stdout."""
    child=subprocess.Popen(args,stdout=subprocess.PIPE,stderr=subprocess.PIPE,
                           start_new_session=True,pass_fds=tuple(fds))
    try:
        out,err=child.communicate(timeout=timeout)
    except BaseException:
        try:os.killpg(child.pid,signal.SIGKILL)
        except ProcessLookupError:pass
        child.communicate()
        raise
    try:os.killpg(child.pid,0)
    except ProcessLookupError:pass
    else:
        os.killpg(child.pid,signal.SIGKILL)
        raise RuntimeError('content reader descendants remain; inspect custody')
    if child.returncode:
        raise subprocess.CalledProcessError(child.returncode,args,stderr=err)
    return out


def run(namespace, mode):
    if mode not in ('--prepare-reviewed-image', '--admit-reviewed-operation'):
        raise ValueError('explicit split operation mode required')
    root, source = namespace['R'], namespace['SOURCE']
    call, preimage, window = namespace['call'], namespace['preimage'], namespace['window']
    release_lease = Path(namespace['RELEASE_LEASE'])
    if (root/'coordinator-started.json').exists():
        raise ValueError('historical operation cannot be adopted')
    hashes=json.loads((root/'execution-hashes.json').read_text())
    identity={'source':source,'inputs_sha256':hashlib.sha256((root/'execution-hashes.json').read_bytes()).hexdigest()}
    sys.path.insert(0,str(root/'release/tools/deploy'))
    from artifact_retention import open_lease, validate_lease
    artifact_held=[None]

    def inputs(fd):
        review=json.loads((root/'execution-review.json').read_text())
        if review['state']!='independently_reviewed' or review['source']!=source or review['hashes']!=hashes:
            raise ValueError('unreviewed operation')
        for name,digest in hashes.items():
            target=root/name
            if target.is_symlink() or not target.is_file() or hashlib.sha256(target.read_bytes()).hexdigest()!=digest:
                raise ValueError('operation input changed: '+name)
        proof=json.loads((root/'canonical-proof.json').read_text())
        if proof['state']!='passed' or proof['source']!=source:
            raise ValueError('canonical proof mismatch')
        prepared_release.validate_custody(fd,release_lease)
        os.environ['FC_PREP_ROLLOUT_FD']=str(fd)
        os.environ['FC_ROLLOUT_FD']=str(fd)
        current=preimage() # Original exact source/image/readiness validation.
        return {'uid':current['metadata']['uid'],'generation':current['metadata']['generation'],
                'revision':current['metadata']['annotations']['deployment.kubernetes.io/revision'],
                'spec':current['spec']}

    def image_content(fd):
        if artifact_held[0] is None:raise ValueError('artifact custody missing')
        validate_lease(artifact_held[0])
        report=prepared_release.read_receipt(root/'image-manifest.json')
        archive=root/'image.tar'
        if archive.is_symlink() or not archive.is_file() or archive.stat().st_size>512*1024*1024:
            raise ValueError('invalid prepared archive')
        digest=hashlib.sha256()
        with archive.open('rb') as src:
            while block:=src.read(1024*1024):digest.update(block)
        def get_blob(value):
            # Same trusted local content-store reader used by the existing image
            # producer, now with explicit per-command timeout and inherited lease.
            return read_blob(['k3s','ctr','-n','k8s.io','content','get',value],
                             (fd,artifact_held[0].fileno()))
        binary=hashes['release/bin/fcd']
        return verify_content(report,source,binary,namespace['IMAGE'],digest.hexdigest(),get_blob)

    def build(fd):
        with open_lease(root/'image-artifact.lock') as artifact:
            fcntl.flock(artifact,fcntl.LOCK_EX|fcntl.LOCK_NB)
            artifact_held[0]=artifact
            os.environ['FC_ARTIFACT_LEASE_FD']=str(artifact.fileno())
            try:
                result=call(['python3','-I',str(root/'image.py')],timeout=180,check=False,fds=(fd,artifact.fileno()))
            finally:
                os.environ.pop('FC_ARTIFACT_LEASE_FD',None)
            (root/'image.stdout').write_text(result.stdout)
            (root/'image.stderr').write_text(result.stderr)
            if result.returncode!=0:raise ValueError('producer failed; never replay')
            image_content(fd)

    def admission_inputs(fd):
        current=inputs(fd)
        image_content(fd)
        return current

    def rollout(fd):
        validate_lease(artifact_held[0])
        # No outer timeout may bypass original reaping/reconciliation.
        try:runpy.run_path(str(root/'rollout.py'),run_name='__main__')
        except SystemExit as end:
            if end.code not in (None,0):raise ValueError('inspect unresolved operation; never replay')
        proof=json.loads((root/'live-proof.json').read_text())
        if proof['source']!=source or proof['state']!='verified_deployed':
            raise ValueError('live proof missing')
        if call(['systemctl','is-active','firecontroller-offsite-batch.timer']).stdout.strip()!='active':
            raise ValueError('timer changed')
        prepared_release.durable_new(root/'coordinator-result.json',{'source':source,'state':'verified_timer_unchanged'})
        return proof

    if mode=='--prepare-reviewed-image':
        return prepared_release.prepare(root,release_lease,identity,inputs,build)
    # Acquire artifact only after release custody, matching preparation order.
    # prepared_release.admit owns that release acquisition; first verify takes
    # the artifact nonblockingly and keeps it until the entire call returns.
    def held_inputs(fd):
        if artifact_held[0] is None:
            artifact=open_lease(root/'image-artifact.lock')
            try:fcntl.flock(artifact,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BaseException:artifact.close();raise
            artifact_held[0]=artifact
        return admission_inputs(fd)
    try:
        return prepared_release.admit(root,release_lease,identity,held_inputs,window,rollout)
    finally:
        if artifact_held[0] is not None:artifact_held[0].close()
