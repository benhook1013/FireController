"""Pure release-custody contract checks. No deployment or deletion authority."""
import hashlib
import json
import re
import os
import stat
import fcntl
import tempfile
from pathlib import Path
from artifact_retention import Refuse
from deployment_duplicates import canonical, private_file, dependency_paths

HASH=re.compile(r"[0-9a-f]{64}")

def validate_state(path, lease_identity, observed):
    """Validate a pinned pointer against caller-provided fresh live observation.

    The caller must hold the same lease and obtain observed from the deployment
    adapter, not from a saved declaration. This module alone cannot authorize GC.
    """
    raw=private_file(path,65536); state=json.loads(raw)
    if set(state)!={"version","generation","lease_identity","current","rollback","pending","node_id","producer","previous_generation"} or state["version"]!=1:
        raise Refuse("unsupported custody state")
    if not isinstance(state["generation"],str) or not re.fullmatch(r"[0-9a-f]{32}",state["generation"]):
        raise Refuse("invalid custody generation")
    if (state["producer"]!="firecontroller-guarded-install-v1" or not isinstance(state["node_id"],str)
        or not re.fullmatch(r"[0-9a-f]{32}",state["node_id"])
        or not isinstance(state["previous_generation"],str)
        or not re.fullmatch(r"[0-9a-f]{32}",state["previous_generation"])
        or state["previous_generation"]==state["generation"]):
        raise Refuse("bound node, producer and predecessor required")
    if state["lease_identity"]!=list(lease_identity):
        raise Refuse("custody lease replaced")
    if not isinstance(observed,dict) or set(observed)!={"generation","current","rollback","pending","node_id","producer","previous_generation"}:
        raise Refuse("complete fresh deployment observation required")
    if observed!={k:state[k] for k in observed}:
        raise Refuse("live deployment observation disagrees")
    protected=set(); identities=set(); hashed={}; verified_bytes=0
    for role in ("current","rollback","pending"):
        pin=state[role]
        if role=="pending" and pin is None:continue
        if not isinstance(pin,dict) or set(pin)!={"path","sha256"} or not HASH.fullmatch(pin.get("sha256", "")):
            raise Refuse("explicit immutable release pin required")
        manifest_path=canonical(pin["path"]);data=private_file(manifest_path,1024*1024)
        if hashlib.sha256(data).hexdigest()!=pin["sha256"]:
            raise Refuse("release manifest changed")
        release=json.loads(data)
        if set(release)!={"version","phase","source","image_digest","roots","dependencies"} or release["version"]!=1:
            raise Refuse("unsupported release manifest")
        if not re.fullmatch(r"[0-9a-f]{40}",release["source"]):
            raise Refuse("exact source required")
        prepared=role=="pending" and release["phase"]=="prepared" and release["image_digest"] is None
        if not prepared and (release["phase"]!="built" or not isinstance(release["image_digest"],str) or not re.fullmatch(r"sha256:[0-9a-f]{64}",release["image_digest"])):
            raise Refuse("built current/rollback or explicit pre-image pending required")
        roots=release["roots"];deps=release["dependencies"]
        if not isinstance(roots,list) or not roots or len(roots)>128 or not isinstance(deps,list) or not deps or len(deps)>128:
            raise Refuse("bounded complete dependencies required")
        for name in roots:
            root=canonical(name)
            if not root.is_dir():raise Refuse("protected release root missing")
            protected.add(str(root))
        declared=set();configs=[]
        for dep in deps:
            if not isinstance(dep,dict) or set(dep)!={"path","sha256","role"} or not HASH.fullmatch(dep.get("sha256", "")):
                raise Refuse("exact dependency required")
            if dep["role"] not in ("executable","config","recovery-reader"):
                raise Refuse("unsupported dependency role; incomplete graph")
            target=canonical(dep["path"])
            if not target.exists():raise Refuse("release dependency missing")
            before=target.lstat()
            if not stat.S_ISREG(before.st_mode):raise Refuse("regular dependency required")
            if before.st_size>512*1024*1024 or verified_bytes+before.st_size>1024*1024*1024:
                raise Refuse("dependency verification budget exhausted")
            key=(str(target),dep["sha256"])
            if key not in hashed:
                fd=os.open(target,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
                with os.fdopen(fd,"rb") as stream:
                    current=os.fstat(stream.fileno())
                    signature=lambda v:(v.st_dev,v.st_ino,v.st_size,v.st_mtime_ns)
                    if signature(before)!=signature(current):raise Refuse("dependency changed")
                    h=hashlib.sha256();count=0
                    while chunk:=stream.read(1024*1024):
                        count+=len(chunk)
                        if count>before.st_size:raise Refuse("dependency grew")
                        h.update(chunk)
                    if count!=before.st_size or signature(os.fstat(stream.fileno()))!=signature(before) or signature(target.stat())!=signature(before):
                        raise Refuse("dependency changed during verification")
                verified_bytes+=count
                if h.hexdigest()!=dep["sha256"]:raise Refuse("dependency bytes changed")
                hashed[key]=True
            declared.add(str(target));protected.add(str(target))
            if dep["role"]=="config":configs.append(str(target))
        if configs:
            closure=dependency_paths(configs)
            for name in closure:
                if Path(name).is_file() and name not in declared:
                    raise Refuse("undeclared indirect file dependency")
                protected.add(name)
        identities.add((str(manifest_path),pin["sha256"]));protected.add(str(manifest_path))
    return {"state_sha256":hashlib.sha256(raw).hexdigest(),"protected_paths":sorted(protected),"release_count":len(identities),"verified_dependency_bytes":verified_bytes,"cleanup_authorized":False}


def publish_prepared(path, lock_path, expected_sha256, proposed, observe):
    """CAS an existing custody pointer to a protected pre-image pending state.

    No bootstrap, commit/rollback promotion, pending clear or GC authorization.
    The installer must supply a genuinely independent live observation adapter.
    """
    from artifact_retention import open_lease
    path=canonical(path);parent=path.parent
    ps=parent.stat()
    if ps.st_uid!=os.geteuid() or ps.st_mode & 0o077:
        raise Refuse("private custody directory required")
    if not isinstance(expected_sha256,str) or not HASH.fullmatch(expected_sha256):
        raise Refuse("exact predecessor digest required")
    with open_lease(lock_path) as lease:
        fcntl.flock(lease,fcntl.LOCK_EX|fcntl.LOCK_NB)
        ls=os.fstat(lease.fileno());identity=(ls.st_dev,ls.st_ino)
        before=path.lstat();old_bytes=private_file(path,65536)
        if hashlib.sha256(old_bytes).hexdigest()!=expected_sha256:
            raise Refuse("custody compare-and-swap conflict")
        old=json.loads(old_bytes)
        old_observed=observe()
        validate_state(path,identity,old_observed)
        if old['pending'] is not None:
            raise Refuse("unresolved pending release protected")
        for field in ('version','node_id','producer','lease_identity','current','rollback'):
            if proposed.get(field)!=old[field]:raise Refuse("prepared transition changed active custody")
        if proposed.get('previous_generation')!=old['generation'] or proposed.get('generation')==old['generation'] or proposed.get('pending') is None:
            raise Refuse("prepared transition requires new generation and pending")
        data=json.dumps(proposed,sort_keys=True,separators=(',',':')).encode()
        if len(data)>65536:raise Refuse("custody state budget exceeded")
        fd,name=tempfile.mkstemp(prefix='.custody-',dir=parent)
        temp=Path(name)
        try:
            with os.fdopen(fd,'wb') as stream:
                stream.write(data);stream.flush();os.fsync(stream.fileno())
            candidate_observed={k:proposed[k] for k in old_observed}
            # This validates candidate graph shape/bytes, not candidate live truth.
            validate_state(temp,identity,candidate_observed)
            pending=json.loads(private_file(proposed['pending']['path'],1024*1024))
            if pending['phase']!='prepared' or pending['image_digest'] is not None:
                raise Refuse("pre-image pending required")
            if observe()!=old_observed:
                raise Refuse("live custody changed during preparation")
            final=path.lstat()
            if (final.st_dev,final.st_ino)!=(before.st_dev,before.st_ino) or private_file(path,65536)!=old_bytes:
                raise Refuse("custody pointer changed before publication")
            if (os.stat(lock_path).st_dev,os.stat(lock_path).st_ino)!=identity:
                raise Refuse("custody lease changed")
            os.replace(temp,path)
            directory_fd=os.open(parent,os.O_RDONLY|os.O_DIRECTORY)
            try:os.fsync(directory_fd)
            finally:os.close(directory_fd)
            return {'state':'prepared_published','generation':proposed['generation'],'state_sha256':hashlib.sha256(data).hexdigest(),'cleanup_authorized':False}
        finally:
            if temp.exists():temp.unlink()
