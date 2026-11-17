#!/usr/bin/env python3
"""Offline operator release assembly. Never install, fetch, or activate services."""
import argparse
import contextlib
import gzip
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import tarfile
import tempfile

ROOT=Path(__file__).resolve().parents[2]
def module(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    result=importlib.util.module_from_spec(spec);spec.loader.exec_module(result)
    return result
clients=module('operator_clients',ROOT/'tools/release/package-clients.py')
health=module('operator_health',ROOT/'tools/health/package_bundle.py')


def package(binary_dir,reader,output,revision):
    if not re.fullmatch('[0-9a-f]{40}',revision):raise ValueError('full source revision required')
    output=Path(output)
    if not output.is_absolute() or output.parent.resolve()!=output.parent:raise ValueError('canonical output parent required')
    if output.exists() or output.is_symlink():raise FileExistsError('output already exists')
    clients.artifact_lease_options()
    # Missing/incompatible legacy readers fail before any native build packaging.
    health.verified_bytes(reader,health.READER_SHA,128<<20)
    health.verified_bytes(ROOT/'tools/health/publish_backup_health.py',health.PUBLISHER_SHA,65536)
    with tempfile.TemporaryDirectory(prefix='.operator-release-',dir=output.parent) as temporary:
        stage=Path(temporary)
        # Existing packager independently checks clean Go VCS metadata and exact
        # checkout revision. No substitution of its daemon for the health reader.
        with contextlib.redirect_stdout(io.StringIO()):
            clients.package(binary_dir,'linux-amd64',stage,revision)
        native=stage/'firecontroller-linux-amd64.tar.gz'
        companion=stage/'operator-health-linux-amd64.tar.gz'
        health_manifest=health.build(reader,companion)
        artifacts={}
        for path in (native,companion):
            if path.stat().st_size>256<<20:raise ValueError('bounded release artifact required')
            body=path.read_bytes()
            artifacts[path.name]=body
        manifest={'version':1,'platform':'linux-amd64','source_revision':revision,
                  'installation_ready':False,'health_reader_sha256':health.READER_SHA,
                  'artifacts':{name:{'sha256':hashlib.sha256(body).hexdigest(),'bytes':len(body)} for name,body in artifacts.items()},
                  'unresolved_prerequisites':health_manifest['prerequisites']}
        artifacts['operator-release.json']=json.dumps(manifest,sort_keys=True,separators=(',',':')).encode()+b'\n'
        archive=stage/'release.tar.gz'
        with archive.open('xb') as stream:
            with gzip.GzipFile(filename='',mode='wb',fileobj=stream,mtime=0) as compressed:
                with tarfile.open(fileobj=compressed,mode='w') as out:
                    for name,body in artifacts.items():
                        info=tarfile.TarInfo(name);info.size=len(body);info.mode=0o644
                        out.addfile(info,io.BytesIO(body))
            stream.flush();os.fsync(stream.fileno())
        os.link(archive,output) # atomic no-replace publication on same filesystem
        fd=os.open(output.parent,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
    return manifest

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary-dir',required=True)
    parser.add_argument('--health-reader',required=True)
    parser.add_argument('--output',required=True)
    parser.add_argument('--revision',required=True)
    args=parser.parse_args();package(args.binary_dir,args.health_reader,args.output,args.revision)
    print(args.output)