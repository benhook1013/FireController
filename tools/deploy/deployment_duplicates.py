#!/usr/bin/env python3
"""Bounded exact-duplicate disposal inside explicitly terminal deployment roots."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
from artifact_retention import Refuse, active_paths, digest, open_lease, retained_by_reference
from artifact_schedule import save_result, validate_result_store


def canonical(path):
    p = Path(path)
    if not p.is_absolute() or p.resolve() != p:
        raise Refuse("canonical absolute path required")
    return p



def private_file(path, limit):
    p=canonical(path)
    before=p.lstat()
    if not stat.S_ISREG(before.st_mode):
        raise Refuse("configuration must be regular")
    fd=os.open(p,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
    with os.fdopen(fd,"rb") as f:
        current=os.fstat(f.fileno())
        if (current.st_dev,current.st_ino)!=(before.st_dev,before.st_ino) or current.st_uid!=os.geteuid() or current.st_mode & 0o077 or current.st_size>limit:
            raise Refuse("unsafe configuration")
        data=f.read(limit+1)
        if len(data)>limit:raise Refuse("configuration budget exceeded")
        return data


def regular_digest(path):
    # Reject FIFO/devices before the inherited blocking hash helper can open them.
    s=path.lstat()
    if not stat.S_ISREG(s.st_mode) or path.resolve()!=path:
        raise Refuse("regular canonical artifact required")
    return digest(path)


def dependency_paths(config_files):
    """Follow explicitly selected private JSON configs; never emit values or secrets."""
    if not isinstance(config_files,list) or not config_files or len(config_files)>128:
        raise Refuse("explicit bounded dependency configurations required")
    pending=[canonical(p) for p in config_files]
    seen=set(); references=set()
    while pending:
        p=pending.pop()
        if p in seen:
            continue
        seen.add(p)
        if len(seen)>128:
            raise Refuse("dependency graph budget exhausted")
        value=json.loads(private_file(p,1024*1024))
        def walk(v):
            if isinstance(v,dict):
                for child in v.values():walk(child)
            elif isinstance(v,list):
                for child in v:walk(child)
            elif isinstance(v,str) and v.startswith("/"):
                target=canonical(v)
                if not target.exists():
                    raise Refuse("configured local dependency is missing")
                references.add(str(target))
                if target.is_file():
                    # Provider JSON filenames are not constrained to .json by PushBatch.
                    # Sniff bounded bytes, then parse full JSON with private-file limits.
                    fd=os.open(target,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
                    with os.fdopen(fd,"rb") as stream:
                        prefix=stream.read(4096).lstrip()
                    if target.suffix==".json" or prefix.startswith((b"{",b"[")):
                        pending.append(target)
                elif not target.is_dir():
                    raise Refuse("nonregular configured dependency")
        walk(value)
        references.add(str(p))
    return references


def validate_producer_receipts(config, lease_identity):
    """Bind enrollment to actual artifact_operation output, never an arbitrary marker."""
    entries=config.get("terminal_roots")
    if not isinstance(entries,list) or len(entries)>128:
        raise Refuse("bounded producer enrollment required")
    for entry in entries:
        directory=canonical(entry["path"])
        manifest_path=canonical(entry["producer_manifest"])
        marker=canonical(entry["marker"])
        if directory not in manifest_path.parents or marker.parent!=directory:
            raise Refuse("producer evidence must belong to enrolled root")
        manifest_bytes=private_file(manifest_path,1024*1024)
        manifest=json.loads(manifest_bytes)
        if manifest.get("workflow_receipt")!=marker.name:
            raise Refuse("producer receipt location mismatch")
        receipt_bytes=private_file(marker,65536)
        if hashlib.sha256(receipt_bytes).hexdigest()!=entry["marker_sha256"]:
            raise Refuse("producer receipt changed")
        receipt=json.loads(receipt_bytes)
        if (receipt.get("state")!="terminal" or receipt.get("root")!=str(directory)
            or receipt.get("manifest_sha256")!=hashlib.sha256(manifest_bytes).hexdigest()
            or receipt.get("lock_identity")!=list(lease_identity)):
            raise Refuse("producer receipt does not establish current lease custody")
        finished=receipt.get("finished_at")
        if type(finished) not in (int,float) or not 0<finished<1e11:
            raise Refuse("invalid producer terminal time")


def collect(config, references, apply=False):
    root = canonical(config["root"])
    protected = [canonical(p) for p in config["protected_roots"]]
    entries = config["terminal_roots"]
    if len(protected)<2 or len(set(protected))!=len(protected) or any(not p.is_dir() for p in protected) or not isinstance(entries, list) or len(entries) > 128:
        raise Refuse("bounded enrollment and protected roots required")
    budget = config.get("max_verify_bytes", 1024**3)
    if type(budget) is not int or not 1 <= budget <= 4*1024**3:
        raise Refuse("invalid byte budget")
    candidates = []
    scanned = 0
    verified = 0
    # Terminal markers are private operator evidence, never inferred from age/name.
    for entry in entries:
        directory = canonical(entry["path"])
        if root not in directory.parents or any(directory == p or p in directory.parents or directory in p.parents for p in protected):
            raise Refuse("terminal enrollment overlaps protected roots")
        marker = canonical(entry["marker"])
        if not stat.S_ISREG(marker.lstat().st_mode) or marker.stat().st_size>65536:
            raise Refuse("bounded regular terminal marker required")
        if verified+marker.stat().st_size>budget:
            raise Refuse("terminal marker verification budget exhausted")
        verified+=marker.stat().st_size
        if directory not in marker.parents or regular_digest(marker)[0] != entry["marker_sha256"]:
            raise Refuse("terminal marker changed")
        for parent, dirs, files in os.walk(directory, followlinks=False):
            for name in dirs + files:
                scanned += 1
                if scanned > 10000:
                    raise Refuse("inventory budget exhausted")
                p = Path(parent)/name
                if p.is_symlink():
                    raise Refuse("symlink in enrolled root")
            for name in files:
                p = Path(parent)/name
                # Only named database rehearsal files and standalone build artifacts.
                if name not in ("canonical.sqlite", "firecontroller.sqlite", "fcd"):
                    continue
                s = p.stat()
                if not stat.S_ISREG(s.st_mode):
                    raise Refuse("nonregular artifact")
                if name.endswith(".sqlite") and any(Path(str(p)+suffix).exists() for suffix in ("-wal", "-shm", "-journal")):
                    continue
                if retained_by_reference(p,references):
                    continue
                candidates.append((p,s))
    if len({str(p) for p,s in candidates}) != len(candidates):
        raise Refuse("overlapping enrollment")
    sizes = {}
    for p,s in candidates:
        sizes.setdefault(s.st_size,[]).append((p,s))
    groups = {}
    for size, items in sorted(sizes.items()):
        if len(items)<2:
            continue
        if verified + size*len(items)>budget:
            continue
        for p,s in items:
            h,current = regular_digest(p)
            if (s.st_dev,s.st_ino,s.st_size,s.st_mtime_ns)!=(current.st_dev,current.st_ino,current.st_size,current.st_mtime_ns):
                raise Refuse("artifact changed during inventory")
            verified += size
            groups.setdefault((size,h),[]).append((p,current))
    removals=[]
    final_reserved=0
    for (size,h), items in groups.items():
        items.sort(key=lambda x:(len(x[0].parts),str(x[0])))
        survivor=items[0][0]
        for p,s in items[1:]:
            if p.stat().st_ino==survivor.stat().st_ino:
                continue
            if verified+final_reserved+2*size>budget:
                continue
            final_reserved+=2*size
            removals.append((p,s,survivor,h))
    reclaimed=0
    if apply:
        for p,s,survivor,h in removals:
            # Recheck both exact bytes immediately before unlink; never remove survivor.
            source_hash,current=regular_digest(p)
            verified+=current.st_size
            survivor_hash,survivor_stat=regular_digest(survivor)
            verified+=survivor_stat.st_size
            if source_hash!=h or survivor_hash!=h or (s.st_dev,s.st_ino,s.st_size,s.st_mtime_ns)!=(current.st_dev,current.st_ino,current.st_size,current.st_mtime_ns):
                raise Refuse("duplicate evidence changed")
            p.unlink()
            if s.st_nlink==1:
                reclaimed += s.st_blocks*512
    return {"state":"complete","mode":"apply" if apply else "dry-run","files_eligible":len(removals),"files_removed":len(removals) if apply else 0,"verified_bytes":verified,"reclaimed_allocated_bytes":reclaimed}


def execute(config_path, lock_path, results):
    results=canonical(results)
    s=results.stat()
    if s.st_uid!=os.geteuid() or s.st_mode & 0o077 or not results.is_dir():
        raise Refuse("private result directory required")
    with open_lease(lock_path) as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        config=json.loads(private_file(config_path,65536))
        if set(config)-{"root","protected_roots","terminal_roots","max_verify_bytes","apply","dependency_configs"} or type(config.get("apply",False)) is not bool:
            raise Refuse("invalid configuration")
        if config.get("apply",False):
            raise Refuse("automatic disposal disabled until authoritative live custody integration")
        validate_result_store(results)
        lease_stat=os.fstat(lock.fileno())
        validate_producer_receipts(config,(lease_stat.st_dev,lease_stat.st_ino))
        references=active_paths()+list(dependency_paths(config.get("dependency_configs")))
        result=collect(config,references,config.get("apply",False))
        save_result(results,result)
        return result


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ("config","lock-file","results"):
        parser.add_argument("--"+name,required=True,type=Path)
    args=parser.parse_args()
    print(json.dumps(execute(args.config,args.lock_file,args.results),sort_keys=True))
