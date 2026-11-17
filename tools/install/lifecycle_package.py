"""Read-only byte qualification for the lifecycle template package.
The expected digest comes from an independently selected release receipt. This
never executes templates, resolves arbitrary code dependencies, or grants switch
permission. Rendered operation and external dependency closure remain mandatory.
"""
import hashlib
import json
import os
from pathlib import PurePosixPath
import re
import stat

FILES = frozenset(name+'.py' for name in (
    'artifact_operation','artifact_retention','bounded_rehearsal','failed_equivalent',
    'image-next-template','image_layers','prepare-successor','prepare_rehearsal',
    'prepare_successor','rehearsal_operation','rollout-next-template',
    'supervised_equivalent','supervised_failure'))
HEX = re.compile(r'[0-9a-f]{64}')
class Invalid(ValueError): pass

def absolute(value):
    if not isinstance(value,str): raise Invalid('canonical absolute path required')
    p=PurePosixPath(value)
    if not p.is_absolute() or str(p)!=value or '..' in p.parts: raise Invalid('canonical absolute path required')
    return p

def open_directory(value):
    p=absolute(value)
    fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
    try:
        for part in p.parts[1:]:
            child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
            os.close(fd);fd=child
        return fd
    except BaseException:
        os.close(fd);raise

def identity(st):
    return (st.st_dev,st.st_ino,st.st_size,st.st_mtime_ns,st.st_ctime_ns,st.st_uid,st.st_gid,st.st_mode)

def read_file(fd,name,limit):
    source=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=fd)
    try:
        before=os.fstat(source)
        if not stat.S_ISREG(before.st_mode) or before.st_size>limit: raise Invalid('bounded regular package file required')
        out=bytearray()
        while len(out)<=limit:
            part=os.read(source,min(65536,limit+1-len(out)))
            if not part:break
            out.extend(part)
        if len(out)>limit or identity(before)!=identity(os.fstat(source)): raise Invalid('package file changed or exceeded limit')
        return bytes(out),identity(before)
    finally:os.close(source)

def inspect(root,expected_manifest_sha256):
    if not isinstance(expected_manifest_sha256,str) or not HEX.fullmatch(expected_manifest_sha256): raise Invalid('pinned manifest digest required')
    fd=open_directory(root)
    try:
        root_id=identity(os.fstat(fd))
        raw,manifest_id=read_file(fd,'manifest.json',1<<20)
        if hashlib.sha256(raw).hexdigest()!=expected_manifest_sha256: raise Invalid('manifest digest mismatch')
        def pairs(items):
            result={}
            for key,value in items:
                if key in result:raise Invalid('duplicate manifest field')
                result[key]=value
            return result
        manifest=json.loads(raw,object_pairs_hook=pairs)
        if not isinstance(manifest,dict) or set(manifest)!={'failed_cleanup_default','installation_path','original_operation','previous_template_source','rehearsal_policy','sha256','source'}: raise Invalid('unrecognized lifecycle manifest')
        if manifest['failed_cleanup_default'] is not False: raise Invalid('cleanup must remain disabled')
        for key in ('installation_path','original_operation'):absolute(manifest[key])
        for key in ('source','previous_template_source'):
            if not isinstance(manifest[key],str) or not re.fullmatch('[0-9a-f]{40}',manifest[key]):raise Invalid('source identity required')
        policy=manifest['rehearsal_policy']
        if not isinstance(policy,dict) or set(policy)!={'budget_lease','lease','root','source_lease','source_lease_uid','source_owner_uid'}:raise Invalid('unrecognized lifecycle policy')
        for key in ('budget_lease','lease','root','source_lease'):absolute(policy[key])
        for key in ('source_lease_uid','source_owner_uid'):
            if type(policy[key]) is not int or not 0<=policy[key]<2**32:raise Invalid('explicit owner required')
        hashes=manifest['sha256']
        if not isinstance(hashes,dict) or set(hashes)!=FILES:raise Invalid('complete template file inventory required')
        identities={'manifest.json':manifest_id};total=0
        for name in sorted(FILES):
            expected=hashes[name]
            if not isinstance(expected,str) or not HEX.fullmatch(expected):raise Invalid('invalid file digest')
            data,item_id=read_file(fd,name,4<<20);total+=len(data)
            if total>32<<20 or hashlib.sha256(data).hexdigest()!=expected:raise Invalid('package digest or budget mismatch')
            identities[name]=item_id
        for name,item_id in identities.items():
            if identity(os.stat(name,dir_fd=fd,follow_symlinks=False))!=item_id:raise Invalid('package identity changed')
        current=open_directory(root)
        try:
            if identity(os.fstat(current))!=root_id:raise Invalid('package root changed')
        finally:os.close(current)
        return {'manifest_sha256':expected_manifest_sha256,'source':manifest['source'],
                'verified_files':len(FILES),'installation_path':manifest['installation_path'],
                'template_original_operation':manifest['original_operation'],
                'declared_policy':policy,'package_bytes_verified':True,
                'rendered_operation_verified':False,'closure_verified':False,'installation_ready':False}
    finally:os.close(fd)
