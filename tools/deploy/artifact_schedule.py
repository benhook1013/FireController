#!/usr/bin/env python3
"""Bounded opt-in collector; private config and release custody read under lease."""
import argparse
import fcntl
import json
import math
import os
from pathlib import Path
import stat
import time
from types import SimpleNamespace

from artifact_retention import Refuse, collect_locked, open_lease


def private_file(path, limit):
    # A FIFO must reach the regular-file check instead of waiting for a writer.
    # O_NONBLOCK does not change ordinary regular-file reads.
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as f:
        s = os.fstat(f.fileno())
        if not stat.S_ISREG(s.st_mode) or s.st_uid != os.geteuid() or s.st_mode & 0o077 or s.st_size > limit:
            raise Refuse("unsafe scheduler state")
        body = f.read(limit + 1)
        if len(body) > limit:
            raise Refuse("oversized scheduler state")
        return body


def configuration(path, now):
    c = json.loads(private_file(path, 16384))
    required = {"root", "manifest", "current_revision", "verified_rollback_revision", "valid_until"}
    optional = {"apply", "max_verify_bytes", "min_free_bytes"}
    if not isinstance(c, dict) or not required <= c.keys() or c.keys() - required - optional:
        raise Refuse("invalid collector configuration")
    if type(c["valid_until"]) not in (int, float) or not math.isfinite(c["valid_until"]) or not now < c["valid_until"] <= now + 86400:
        raise Refuse("release custody configuration expired or unbounded")
    if type(c.get("apply", False)) is not bool:
        raise Refuse("invalid collection mode")
    for field, default, minimum, maximum in (("max_verify_bytes", 1024**3, 1, 1024**3), ("min_free_bytes", 0, 0, 2**63-1)):
        value = c.get(field, default)
        if type(value) is not int or not minimum <= value <= maximum:
            raise Refuse("invalid collector budget")
        c[field] = value
    for field in ("root", "manifest"):
        if not isinstance(c[field], str) or not Path(c[field]).is_absolute() or str(Path(c[field]).resolve()) != c[field]:
            raise Refuse("collector paths must be canonical and absolute")
    c["root"], c["manifest"] = Path(c["root"]), Path(c["manifest"])
    c["apply"] = c.get("apply", False)
    c.pop("valid_until")
    return SimpleNamespace(**c)


def validate_result_store(directory):
    for name in ("latest.json", "previous.json"):
        path = directory / name
        if path.exists() or path.is_symlink():
            private_file(path, 4096)
    pending = directory / "pending.json"
    if pending.exists() or pending.is_symlink():
        raise Refuse("unfinished collector result requires inspection")


def save_result(directory, result):
    # Fixed latest/previous snapshots, never an unbounded per-run history.
    body = (json.dumps(result, sort_keys=True) + "\n").encode()
    if len(body) > 4096:
        raise Refuse("oversized collector result")
    latest, previous, pending = (directory / n for n in ("latest.json", "previous.json", "pending.json"))
    for path in (latest, previous):
        if path.exists() or path.is_symlink():
            private_file(path, 4096)
    fd = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(body)
        f.flush()
        os.fsync(f.fileno())
    if latest.exists():
        os.replace(latest, previous)
    os.replace(pending, latest)
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def execute(config, lock_path, results):
    results = Path(results)
    s = results.lstat()
    if results.resolve() != results or not stat.S_ISDIR(s.st_mode) or s.st_uid != os.geteuid() or s.st_mode & 0o077:
        raise Refuse("private canonical result directory required")
    for metadata in (Path(config), Path(lock_path)):
        resolved = metadata.resolve()
        if results == resolved or results in resolved.parents:
            raise Refuse("scheduler metadata must be outside result directory")
    # No waiting behind an active producer; systemd retries only on next timer.
    with open_lease(lock_path) as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        started = time.time()
        result = {"started_at": started, "state": "refused"}
        safe_to_save = False
        try:
            a = configuration(config, started)
            if results == a.root or a.root in results.parents:
                raise Refuse("result directory must be outside enrolled root")
            validate_result_store(results)
            safe_to_save = True
            detail = collect_locked(a, lock)
            result.update({"state": "complete", "mode": detail["mode"], "files_removed": sum(f["state"] == "removed" for f in detail["files"]),
                           "files_eligible": sum(f["state"] == "eligible" for f in detail["files"]), "verified_bytes": detail["verified_bytes"],
                           "reclaimed_allocated_bytes": detail["reclaimed_allocated_bytes"], "free_bytes": detail["free_bytes"], "headroom_alert": detail["headroom_alert"]})
        except Exception:
            # Never retain arbitrary exception strings, private paths or captures.
            result["finished_at"] = time.time()
            if safe_to_save:
                save_result(results, result)
            raise
        result["finished_at"] = time.time()
        save_result(results, result)
        return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--config", type=Path, required=True)
    p.add_argument("--lock-file", type=Path, required=True)
    p.add_argument("--results", type=Path, required=True)
    a = p.parse_args()
    try:
        result = execute(a.config, a.lock_file, a.results)
    except Exception:
        print('{"state":"refused","action":"inspect collector custody and latest result"}')
        raise SystemExit(1)
    print(json.dumps(result, sort_keys=True))
    if result["headroom_alert"]:
        raise SystemExit(2)


if __name__ == "__main__":
    main()
