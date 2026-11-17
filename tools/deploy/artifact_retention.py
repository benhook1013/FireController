#!/usr/bin/env python3
"""Bounded, opt-in retention of redundant operator files. Dry-run by default."""
import argparse
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import stat
import time


class Refuse(ValueError):
    pass


def validate_lease(lease):
    s = os.fstat(lease.fileno())
    if not stat.S_ISREG(s.st_mode) or s.st_uid != os.geteuid() or s.st_mode & 0o022:
        raise Refuse("lease must be a private operator-owned regular file")


def open_lease(path):
    # Inspect first so a FIFO cannot block open; NONBLOCK also covers replacement
    # between lstat and open. Ancestors must resolve to this exact canonical path.
    path = Path(path)
    if not path.is_absolute() or path.resolve() != path:
        raise Refuse("lease must have a canonical absolute path")
    before = path.lstat()
    if not stat.S_ISREG(before.st_mode) or before.st_uid != os.geteuid() or before.st_mode & 0o022:
        raise Refuse("unsafe existing lease")
    fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
    lease = os.fdopen(fd, "r+")
    try:
        validate_lease(lease)
        opened = os.fstat(fd)
        if (before.st_dev, before.st_ino) != (opened.st_dev, opened.st_ino):
            raise Refuse("lease changed during open")
    except Exception:
        lease.close()
        raise
    return lease


def canonical_relative(relative):
    if (not isinstance(relative, str) or not relative or
            Path(relative).as_posix() != relative or "\\" in relative or
            Path(relative).is_absolute() or any(p in (".", "..") for p in relative.split("/"))):
        raise Refuse("non-canonical relative artifact path")
    return relative


def regular(root, relative):
    canonical_relative(relative)
    parts = Path(relative).parts
    if not parts or Path(relative).is_absolute() or any(p in (".", "..") for p in parts):
        raise Refuse("invalid relative artifact path")
    current = root
    for part in parts:
        current = current / part
        s = current.lstat()
        if stat.S_ISLNK(s.st_mode):
            raise Refuse("symlink artifact path")
    if not stat.S_ISREG(s.st_mode):
        raise Refuse("artifact must be a regular file")
    return current, s


def digest(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as stream:
        before = os.fstat(stream.fileno())
        h = hashlib.sha256()
        for block in iter(lambda: stream.read(65536), b""):
            h.update(block)
        after = os.fstat(stream.fileno())
    if (before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_ino, after.st_size, after.st_mtime_ns):
        raise Refuse("artifact changed during verification")
    return h.hexdigest(), after


def mapping_links(proc):
    try:
        return list((proc / "map_files").iterdir())
    except FileNotFoundError:
        if proc.exists():
            raise Refuse("mapping inventory unavailable for a live process")
        return []  # Process exited during inventory.


def active_paths():
    """Retain open/mapped files, process directories and mount trees."""
    result = []
    for proc in Path("/proc").iterdir():
        if not proc.name.isdigit():
            continue
        try:
            # A file-backed mmap can survive closing every descriptor. Kernel
            # map_files links preserve the actual pathname (including whitespace),
            # unlike parsing the escaped/ambiguous textual /proc/PID/maps column.
            entries = ([proc / "cwd", proc / "exe"] +
                       list((proc / "fd").iterdir()) +
                       mapping_links(proc))
            for item in entries:
                try:
                    result.append(os.readlink(item).removesuffix(" (deleted)"))
                except FileNotFoundError:
                    pass  # Process exited or descriptor closed.
        except FileNotFoundError:
            pass
        except PermissionError as exc:
            raise Refuse("cannot establish complete active-process inventory") from exc
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        # Decode exactly the kernel mountinfo escapes in one pass. In
        # particular a literal backslash followed by 012 is not a newline.
        escapes = {"040": " ", "011": "\t", "012": "\n", "134": "\\"}
        result.append(re.sub(r"\\(040|011|012|134)", lambda m: escapes[m[1]], line.split()[4]))
    return result


def retained_by_reference(path, references):
    # A cwd/mount that contains the artifact may have a future dependency on it.
    # Ignore filesystem root; it would protect every file unconditionally.
    return any(r != "/" and (str(path) == r or str(path).startswith(r.rstrip("/") + "/")) for r in references)


def run(root, manifest, current, rollback, references, *, apply=False,
        now=None, max_verify_bytes=1024**3, min_free_bytes=0):
    now = time.time() if now is None else now
    root = Path(root)
    if root.resolve() != root or not root.is_dir():
        raise Refuse("root must be a canonical directory")
    if not re.fullmatch(r"[0-9a-f]{40}", current) or not re.fullmatch(r"[0-9a-f]{40}", rollback):
        raise Refuse("current and verified rollback revisions are required")
    if current == rollback:
        raise Refuse("current and previous rollback must be distinct")
    entries = manifest.get("artifacts")
    if manifest.get("version") != 1 or not isinstance(entries, list) or len(entries) > 1024:
        raise Refuse("invalid or oversized enrollment manifest")
    for e in entries:
        if (not isinstance(e, dict) or not isinstance(e.get("revision"), str) or
                not re.fullmatch(r"[0-9a-f]{40}", e["revision"])):
            raise Refuse("enrollment revision is required")
        if type(e.get("expires_at")) not in (int, float) or not math.isfinite(e["expires_at"]):
            raise Refuse("finite enrollment expiry is required")
        canonical_relative(e.get("path"))
        canonical_relative(e.get("recovery_path"))
    paths = [e["path"] for e in entries]
    if len(set(paths)) != len(paths):
        raise Refuse("invalid or duplicate enrollment")
    # A recovery copy must never be another deletion candidate in this run.
    recovery_paths = {e.get("recovery_path") for e in entries}
    if set(paths) & recovery_paths:
        raise Refuse("recovery copies overlap deletion candidates")
    source_identities = set()
    for e in entries:
        try:
            _, s = regular(root, e["path"])
            if (s.st_dev, s.st_ino) in source_identities:
                raise Refuse("enrolled sources alias the same file")
            source_identities.add((s.st_dev, s.st_ino))
        except FileNotFoundError:
            pass
    result = {"mode": "apply" if apply else "dry-run", "files": [],
              "verified_bytes": 0, "reclaimed_allocated_bytes": 0}
    removals = []
    for e in entries:
        item = {"path": e["path"], "state": "retained"}
        result["files"].append(item)
        if e.get("revision") in (current, rollback):
            item["reason"] = "current_or_rollback"
            continue
        if (e.get("kind") not in ("build", "rehearsal-copy") or
                e.get("terminal") is not True or type(e.get("expires_at")) not in (int, float) or
                not math.isfinite(e["expires_at"]) or e["expires_at"] > now):
            item["reason"] = "not_expired_terminal_enrollment"
            continue
        if not isinstance(e.get("recovery_path"), str) or not re.fullmatch(r"[0-9a-f]{64}", e.get("sha256", "")):
            raise Refuse("verified equivalent recovery copy is required")
        try:
            path, source_stat = regular(root, e["path"])
        except FileNotFoundError:
            item["reason"] = "already_absent"
            continue
        recovery, recovery_stat = regular(root, e["recovery_path"])
        if (recovery_stat.st_dev, recovery_stat.st_ino) in source_identities:
            raise Refuse("recovery copy aliases a deletion candidate")
        if retained_by_reference(path, references):
            item["reason"] = "active_reference"
            continue
        cost = source_stat.st_size + recovery_stat.st_size
        if result["verified_bytes"] + cost > max_verify_bytes:
            item["reason"] = "verification_budget"
            continue
        result["verified_bytes"] += cost
        source_hash, verified_stat = digest(path)
        recovery_hash, _ = digest(recovery)
        if source_hash != e["sha256"] or recovery_hash != source_hash:
            raise Refuse("equivalent-copy hash verification failed")
        if (verified_stat.st_dev, verified_stat.st_ino, verified_stat.st_size, verified_stat.st_mtime_ns) != (
                source_stat.st_dev, source_stat.st_ino, source_stat.st_size, source_stat.st_mtime_ns):
            raise Refuse("artifact changed since enrollment selection")
        item["state"] = "eligible"
        removals.append((path, verified_stat, item))
    # Complete validation before deleting any selected file.
    if apply:
        for path, verified_stat, item in removals:
            # All producers must hold the same lock, including recovery-copy writers.
            last = path.lstat()
            if (last.st_ino, last.st_dev, last.st_size, last.st_mtime_ns) != (
                    verified_stat.st_ino, verified_stat.st_dev, verified_stat.st_size, verified_stat.st_mtime_ns):
                raise Refuse("artifact changed before removal")
            path.unlink()
            item["state"] = "removed"
            if last.st_nlink == 1:
                result["reclaimed_allocated_bytes"] += last.st_blocks * 512
    fs = os.statvfs(root)
    result["free_bytes"] = fs.f_bavail * fs.f_frsize
    result["headroom_alert"] = result["free_bytes"] < min_free_bytes
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--root", type=Path, required=True)
    p.add_argument("--manifest", type=Path, required=True)
    p.add_argument("--lock-file", type=Path, required=True)
    p.add_argument("--current-revision", required=True)
    p.add_argument("--verified-rollback-revision", required=True)
    p.add_argument("--max-verify-bytes", type=int, default=1024**3)
    p.add_argument("--min-free-bytes", type=int, default=0)
    p.add_argument("--apply", action="store_true")
    a = p.parse_args()
    if a.max_verify_bytes <= 0 or a.min_free_bytes < 0:
        p.error("invalid byte budget")
    # Use an existing operator-owned lock; never silently create a different lock.
    with open_lease(a.lock_file) as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        print(json.dumps(collect_locked(a, lock), sort_keys=True))


def collect_locked(a, lock):
    """Caller holds the existing exclusive artifact lease throughout this call."""
    validate_lease(lock)
    if a.manifest.stat().st_size > 1024**2:
        raise Refuse("enrollment manifest exceeds one MiB")
    manifest = json.loads(a.manifest.read_text())
    receipt_path, receipt_stat = regular(a.root, manifest.get("workflow_receipt"))
    if receipt_stat.st_size > 16384 or receipt_stat.st_uid != os.geteuid() or receipt_stat.st_mode & 0o022:
        raise Refuse("unsafe workflow receipt")
    receipt = json.loads(receipt_path.read_text())
    lock_stat = os.fstat(lock.fileno())
    manifest_stat = a.manifest.stat()
    protected_metadata = {(s.st_dev, s.st_ino) for s in (receipt_stat, lock_stat, manifest_stat)}
    expected = hashlib.sha256(a.manifest.read_bytes()).hexdigest()
    if (receipt.get("state") != "terminal" or receipt.get("manifest_sha256") != expected or
            receipt.get("root") != str(a.root.resolve()) or
            receipt.get("lock_identity") != [lock_stat.st_dev, lock_stat.st_ino]):
        raise Refuse("managed-operation terminal receipt does not match")
    for e in manifest.get("artifacts", []):
        try:
            _, s = regular(a.root, e["path"])
            if (s.st_dev, s.st_ino) in protected_metadata:
                raise Refuse("workflow metadata cannot be a cleanup artifact")
        except FileNotFoundError:
            pass
    return run(a.root, manifest, a.current_revision, a.verified_rollback_revision,
        active_paths(), apply=a.apply, max_verify_bytes=a.max_verify_bytes,
        min_free_bytes=a.min_free_bytes)


if __name__ == "__main__":
    main()
