"""Build a bounded operator bundle; never install, fetch or run its contents."""
import argparse
import gzip
import hashlib
import io
import json
import os
import pathlib
import stat
import tarfile
import tempfile

PUBLISHER_SHA='d7b097884fef19d102b69485d92d35c355f3e3fddd6d080793a0a79e24eb32b7'
READER_SHA='987f8f1ad5c77c573b4f0d7de9bff15328085e406deae6246a04fc21e987596c'


def verified_bytes(path,expected,limit):
    path=pathlib.Path(path)
    if not path.is_absolute() or path.resolve()!=path:raise ValueError('canonical absolute input required')
    before=path.lstat()
    if not stat.S_ISREG(before.st_mode) or before.st_size>limit:raise ValueError('bounded regular input required')
    with path.open('rb') as stream:
        opened=os.fstat(stream.fileno())
        if (opened.st_dev,opened.st_ino)!=(before.st_dev,before.st_ino):raise ValueError('input identity changed')
        body=stream.read(limit+1)
    if len(body)>limit or hashlib.sha256(body).hexdigest()!=expected:raise ValueError('qualified input hash mismatch')
    return body


def build(helper,output,publisher=None):
    publisher=pathlib.Path(publisher) if publisher else pathlib.Path(__file__).resolve().with_name('publish_backup_health.py')
    script=verified_bytes(publisher,PUBLISHER_SHA,65536)
    reader=verified_bytes(helper,READER_SHA,128<<20)
    output=pathlib.Path(output)
    if not output.is_absolute() or output.parent.resolve()!=output.parent:raise ValueError('canonical output parent required')
    manifest={'version':1,'platform':'linux-amd64','installation_ready':False,
        'files':{'publish_backup_health.py':{'sha256':PUBLISHER_SHA,'bytes':len(script)},'bin/health-reader':{'sha256':READER_SHA,'bytes':len(reader)}},
        'launch':['/usr/bin/python3','publish_backup_health.py','--fcd','<absolute bundle>/bin/health-reader','--sha256',READER_SHA],
        'prerequisites':['qualified system Python standard library and shared libraries','root operator identity','private canonical provider configurations and ledgers','staging directory root:root 0700','report directory 65532:65532 0700','qualified service/timer/environment and rollout custody'],
        'config_root':'/var/lib/firecontroller/operator-offsite-v1','staging':'/var/lib/firecontroller/operator-health-staging','reports':'/var/lib/firecontroller/operator-health'}
    payload=json.dumps(manifest,sort_keys=True,separators=(',',':')).encode()+b'\n'
    temporary=None
    try:
        with tempfile.NamedTemporaryFile(prefix='.health-bundle-',dir=output.parent,delete=False) as stream:
            temporary=pathlib.Path(stream.name)
            with gzip.GzipFile(filename='',mode='wb',fileobj=stream,mtime=0) as compressed:
                with tarfile.open(fileobj=compressed,mode='w') as archive:
                    for name,body,mode in [('manifest.json',payload,0o644),('publish_backup_health.py',script,0o644),('bin/health-reader',reader,0o755)]:
                        info=tarfile.TarInfo(name);info.size=len(body);info.mode=mode
                        archive.addfile(info,io.BytesIO(body))
            stream.flush();os.fsync(stream.fileno())
        # Atomic no-replace publication. Existing output (even a symlink) is never
        # overwritten. Private cooperating build directory is a caller prerequisite.
        os.link(temporary,output)
        fd=os.open(output.parent,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
    finally:
        if temporary is not None:temporary.unlink()
    return manifest

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--helper',required=True);parser.add_argument('--output',required=True)
    args=parser.parse_args();build(args.helper,args.output)