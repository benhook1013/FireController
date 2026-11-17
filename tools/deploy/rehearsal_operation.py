#!/usr/bin/env python3
"""Create a fresh, leased migration rehearsal and retain replay provenance.

This is not a collector: it never enrolls or adopts existing output.
Explicit disposable mode removes only this successful invocation's temporary DB.
The trusted foreground runner must preserve the lease in every descendant.
"""
import argparse
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import time

from artifact_retention import Refuse, digest, open_lease


MAX_REPORT = 1024 * 1024


def checked_file(path, expected, limit, owner_uid=None):
    path = Path(path)
    if not path.is_absolute() or path.resolve() != path:
        raise Refuse("input must be canonical")
    s = path.lstat()
    if (not stat.S_ISREG(s.st_mode) or s.st_uid != (os.geteuid() if owner_uid is None else owner_uid) or s.st_mode & 0o022
            or s.st_size > limit):
        raise Refuse("unsafe or oversized input")
    actual, s = digest(path)
    if actual != expected:
        raise Refuse("input identity mismatch")
    return path, s


def new_json(path, data):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as out:
        json.dump(data, out, sort_keys=True)
        out.flush()
        os.fsync(out.fileno())
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def checked_report(path, revision):
    s = path.lstat()
    if (not stat.S_ISREG(s.st_mode) or s.st_uid != os.geteuid() or s.st_mode & 0o022
            or s.st_size > MAX_REPORT):
        raise Refuse("unsafe rehearsal report")
    body = path.read_bytes()
    report = json.loads(body)
    expected = {"state": "passed", "source_revision": revision,
                "harness": "candidate-specific-schema-init-v1",
                "existing_table_counts_unchanged": True,
                "existing_table_logical_digests_unchanged": True,
                "provider_workers_constructed": False}
    if not isinstance(report, dict) or set(report) != set(expected) | {"tables"}:
        raise Refuse("unexpected rehearsal report")
    for key, value in expected.items():
        if type(report[key]) is not type(value) or report[key] != value:
            raise Refuse("rehearsal proof refused")
    tables = report["tables"]
    if not isinstance(tables, dict) or not tables or len(tables) > 4096:
        raise Refuse("bounded nonempty table proof required")
    for name, proof in tables.items():
        if (not isinstance(name, str) or not name or len(name) > 1024
                or not isinstance(proof, dict) or set(proof) != {"rows", "sha256"}
                or type(proof["rows"]) is not int or proof["rows"] < 0
                or not isinstance(proof["sha256"], str)
                or not re.fullmatch(r"[0-9a-f]{64}", proof["sha256"])):
            raise Refuse("invalid table proof")
    semantic = hashlib.sha256(json.dumps(tables, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    return hashlib.sha256(body).hexdigest(), semantic


def supervised_command(work, lease, lock_stat):
    # Exact existing deployment isolation; no host-root fallback and no timer.
    for path in (work, lease):
        if not re.fullmatch(r"/[A-Za-z0-9_./-]+", str(path)):
            raise Refuse("supervised paths require literal systemd-safe characters")
    unit = "fc-rehearsal-" + hashlib.sha256(str(work).encode()).hexdigest()[:24]
    return ["/usr/bin/systemd-run", "--unit=" + unit, "--wait", "--pipe", "--collect",
            "--property=Type=exec", "--property=RootDirectory=" + str(work),
            "--property=WorkingDirectory=/", "--property=CPUQuota=100%",
            "--property=MemoryMax=512M", "--property=MemorySwapMax=0",
            "--property=RuntimeMaxSec=180", "--property=TimeoutStopSec=5",
            "--property=KillMode=control-group", "--property=PrivateNetwork=yes",
            "--property=PrivateDevices=yes", "--property=MountAPIVFS=yes",
            "--property=NoNewPrivileges=yes", "--property=BindPaths=" + str(lease) + ":/artifact.lease",
            "/migration-runner", "--data-dir", "/data", "--report", "/rehearsal.json",
            "--lease-file", "/artifact.lease", "--lease-device", str(lock_stat.st_dev),
            "--lease-inode", str(lock_stat.st_ino)]


def execute(root, lease, source, source_sha256, runner, runner_sha256, revision,
            operation, *, timeout=180, max_database_bytes=1024**3, reserve_bytes=1024**3, supervised=False, discard_passed_output=False, source_owner_uid=None):
    if type(discard_passed_output) is not bool:
        raise Refuse("explicit disposable-output mode required")
    if type(supervised) is not bool:
        raise Refuse("explicit supervisor mode required")
    if supervised and timeout != 180:
        raise Refuse("supervised deadline is fixed")
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise Refuse("exact revision required")
    if not re.fullmatch(r"[a-zA-Z0-9_-]{1,80}", operation):
        raise Refuse("new operation identity required")
    if any(not isinstance(v, str) or not re.fullmatch(r"[0-9a-f]{64}", v)
           for v in (source_sha256, runner_sha256)):
        raise Refuse("exact input hashes required")
    if (type(timeout) not in (int, float) or not math.isfinite(timeout) or not 0 < timeout <= 600
            or type(max_database_bytes) is not int or not 0 < max_database_bytes <= 8 * 1024**3
            or type(reserve_bytes) is not int or reserve_bytes < 0):
        raise Refuse("bounded operation budgets required")
    root = Path(root)
    if not root.is_absolute() or root.resolve() != root:
        raise Refuse("canonical operation root required")
    st = root.stat()
    if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.geteuid() or st.st_mode & 0o077:
        raise Refuse("private operator root required")
    with open_lease(lease) as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        source, source_stat = checked_file(source, source_sha256, max_database_bytes, source_owner_uid)
        runner, runner_stat = checked_file(runner, runner_sha256, 128 * 1024**2)
        if not os.access(runner, os.X_OK):
            raise Refuse("runner is not executable")
        # Inputs and lease are installation-owned outside disposable work roots.
        for path in (source, runner, Path(lease)):
            if path == root or root in path.parents:
                raise Refuse("protected input overlaps operation root")
        if (source_stat.st_dev, source_stat.st_ino) == (runner_stat.st_dev, runner_stat.st_ino):
            raise Refuse("protected inputs alias")
        for suffix in ("-wal", "-shm", "-journal"):
            if Path(str(source) + suffix).exists():
                raise Refuse("standalone snapshot required")
        fs = os.statvfs(root)
        if fs.f_bavail * fs.f_frsize < reserve_bytes + max_database_bytes + MAX_REPORT + runner_stat.st_size:
            raise Refuse("insufficient rehearsal headroom")
        work = root / operation
        work.mkdir(mode=0o700)  # Existing, failed and crashed operations are never adopted.
        lock_stat = os.fstat(lock.fileno())
        identity = {"version": 1, "kind": "replayable-rehearsal-not-enrolled",
                    "source_path": str(source), "source_sha256": source_sha256,
                    "runner_path": str(runner), "runner_sha256": runner_sha256,
                    "source_revision": revision, "operation": operation,
                    "root": str(root), "lease_identity": [lock_stat.st_dev, lock_stat.st_ino]}
        new_json(work / "started.json", identity)
        data = work / "data"
        data.mkdir(mode=0o700)
        copied = data / "firecontroller.sqlite"
        fd = os.open(copied, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as output, source.open("rb") as original:
            shutil.copyfileobj(original, output, 65536)
            output.flush()
            os.fsync(output.fileno())
        checked_file(copied, source_sha256, max_database_bytes)
        report = work / "rehearsal.json"
        if supervised:
            # Staging and finalization hold the host lease. The actual static runner
            # acquires the same bind-mounted inode inside the service's RootDirectory.
            # There is no terminal receipt (and no enrollment) across either handoff.
            staged_runner = work / "migration-runner"
            with staged_runner.open("xb") as output, runner.open("rb") as original:
                shutil.copyfileobj(original, output, 65536)
                output.flush()
                os.fsync(output.fileno())
            staged_runner.chmod(0o500)
            checked_file(staged_runner, runner_sha256, 128 * 1024**2)
            (work / "tmp").mkdir(mode=0o700)
            command = supervised_command(work, Path(lease), lock_stat)
            fcntl.flock(lock, fcntl.LOCK_UN)
            try:
                result = subprocess.run(command, stdout=subprocess.DEVNULL,
                                        stderr=subprocess.DEVNULL, timeout=210)
                code = result.returncode
            except subprocess.TimeoutExpired:
                # Do not rerun or assume client death stopped the external service.
                # RuntimeMaxSec/KillMode remain systemd's independent responsibility.
                raise Refuse("supervisor outcome unresolved") from None
            finally:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            current_lease = Path(lease).lstat()
            if (current_lease.st_dev, current_lease.st_ino) != (lock_stat.st_dev, lock_stat.st_ino):
                raise Refuse("supervised lease identity changed")
            checked_file(staged_runner, runner_sha256, 128 * 1024**2)
        else:
            process = subprocess.Popen([str(runner), "--data-dir", str(data), "--report", str(report)],
                                       cwd=work, start_new_session=True, pass_fds=(lock.fileno(),),
                                       env=dict(os.environ, FC_ARTIFACT_LEASE_FD=str(lock.fileno())),
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                code = process.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
                raise Refuse("rehearsal deadline exceeded") from None
            try:
                os.killpg(process.pid, 0)
            except ProcessLookupError:
                pass
            else:
                raise Refuse("rehearsal left descendants")
        if code != 0:
            raise Refuse("rehearsal failed")
        # A successful consumer may change DB bytes; only replay provenance is claimed.
        checked_file(source, source_sha256, max_database_bytes, source_owner_uid)
        checked_file(runner, runner_sha256, 128 * 1024**2)
        for suffix in ("-wal", "-shm", "-journal"):
            if Path(str(source) + suffix).exists():
                raise Refuse("recovery source acquired a SQLite sidecar")
        if sorted(p.name for p in data.iterdir()) != ["firecontroller.sqlite"]:
            raise Refuse("unexpected rehearsal data or active SQLite sidecar")
        output_sha, output_stat = digest(copied)
        checked_file(copied, output_sha, max_database_bytes)
        if (output_stat.st_dev, output_stat.st_ino) == (source_stat.st_dev, source_stat.st_ino):
            raise Refuse("output aliases recovery source")
        report_sha, semantic_sha = checked_report(report, revision)
        terminal = dict(identity, state="passed", finished_at=time.time(),
                        output_sha256=output_sha, report_sha256=report_sha,
                        logical_tables_sha256=semantic_sha, byte_equivalent_recovery=False)
        new_json(work / "terminal.json", terminal)
        if discard_passed_output:
            # Only the freshly created output is eligible. Historical, failed or
            # uncertain operations are never adopted or recursively collected.
            # This is a disposable-test policy, NOT byte-equivalent recovery.
            current, current_stat = checked_file(copied, output_sha, max_database_bytes)
            if (current_stat.st_dev, current_stat.st_ino) != (output_stat.st_dev, output_stat.st_ino):
                raise Refuse("temporary output identity changed")
            if current_stat.st_nlink != 1:
                raise Refuse("temporary output acquired another link")
            current.unlink()
            fd = os.open(data, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
            new_json(work / "disposal.json", {
                "state": "passed_temporary_database_discarded",
                "policy": "disposable-successful-rehearsal-v1",
                "output_sha256": output_sha, "logical_tables_sha256": semantic_sha,
                "source_sha256": source_sha256, "runner_sha256": runner_sha256,
                "logical_bytes_discarded": current_stat.st_size,
                "byte_equivalent_recovery": False})
        return {"state": "passed", "operation": operation, "enrolled": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("root", "lease", "source", "source-sha256", "runner", "runner-sha256", "revision", "operation"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--supervised", action="store_true")
    parser.add_argument("--discard-passed-output", action="store_true",
                        help="discard only the new successful temporary database; preserve proofs and inputs")
    args = vars(parser.parse_args())
    print(json.dumps(execute(**args), sort_keys=True))


if __name__ == "__main__":
    main()
