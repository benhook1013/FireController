#!/usr/bin/env python3
"""Run an enrolled operation or consumer under the artifact-retention lease."""
import argparse
import fcntl
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import time

from artifact_retention import Refuse, canonical_relative, open_lease, validate_lease


def lease_subprocess_options(*, required=False):
    """Preserve managed custody through a foreground local subprocess boundary."""
    raw = os.environ.get("FC_ARTIFACT_LEASE_FD")
    if raw is None and not required:
        return {}
    if raw is None or not re.fullmatch(r"[0-9]{1,10}", raw) or int(raw) < 3:
        raise Refuse("valid inherited artifact lease required")
    fd = int(raw)
    try:
        with os.fdopen(os.dup(fd), "r+") as lease:
            validate_lease(lease)
    except (OSError, OverflowError) as error:
        raise Refuse("unavailable inherited artifact lease") from error
    return {"pass_fds": (fd,)}


def execute(root, lock_path, command, manifest_path=None, consumer=False, producer_only=False):
    if consumer and producer_only:
        raise Refuse("consumer and unenrolled producer modes are exclusive")
    root = Path(root)
    if root.resolve() != root or not root.is_dir() or not command:
        raise Refuse("canonical existing root and command required")
    with open_lease(lock_path) as lock:
        fcntl.flock(lock, (fcntl.LOCK_SH if consumer else fcntl.LOCK_EX) | fcntl.LOCK_NB)
        lock_stat = os.fstat(lock.fileno())
        if consumer or producer_only:
            if manifest_path is not None:
                raise Refuse("unenrolled operations cannot issue terminal receipts")
        else:
            if manifest_path is None or Path(manifest_path).stat().st_size > 1024**2:
                raise Refuse("bounded enrollment manifest required")
            data = Path(manifest_path).read_bytes()
            manifest = json.loads(data)
            relative = canonical_relative(manifest.get("workflow_receipt"))
            # Receipt is top-level and new, never an alias to an earlier operation.
            if "/" in relative:
                raise Refuse("receipt must be a new top-level file")
            receipt_path = root / relative
            if receipt_path.exists() or receipt_path.is_symlink():
                raise Refuse("terminal receipt already exists")
        env = dict(os.environ, FC_ARTIFACT_LEASE_FD=str(lock.fileno()))
        process = subprocess.Popen(command, cwd=root, start_new_session=True,
                                   pass_fds=(lock.fileno(),), env=env)
        code = process.wait()
        try:
            os.killpg(process.pid, 0)
        except ProcessLookupError:
            pass
        else:
            raise Refuse("operation left descendants; no terminal receipt issued")
        if code != 0:
            raise Refuse("operation failed; no terminal receipt issued")
        if consumer or producer_only:
            return {"state": "consumer_finished" if consumer else "producer_finished_unenrolled"}
        if Path(manifest_path).read_bytes() != data:
            raise Refuse("enrollment changed during operation")
        result = {"state": "terminal", "finished_at": time.time(), "root": str(root),
                  "manifest_sha256": hashlib.sha256(data).hexdigest(),
                  "lock_identity": [lock_stat.st_dev, lock_stat.st_ino]}
        fd = os.open(receipt_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as output:
            json.dump(result, output, sort_keys=True)
            output.flush()
            os.fsync(output.fileno())
        return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--root", required=True, type=Path)
    p.add_argument("--lock-file", required=True, type=Path)
    p.add_argument("--manifest", type=Path)
    modes = p.add_mutually_exclusive_group()
    modes.add_argument("--consumer", action="store_true")
    modes.add_argument("--producer-only", action="store_true",
                       help="exclusive custody only; no enrollment or terminal receipt")
    p.add_argument("command", nargs=argparse.REMAINDER)
    a = p.parse_args()
    command = a.command[1:] if a.command[:1] == ["--"] else a.command
    print(json.dumps(execute(a.root, a.lock_file, command, a.manifest, a.consumer, a.producer_only)))


if __name__ == "__main__":
    main()
