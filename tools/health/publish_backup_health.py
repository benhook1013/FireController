#!/usr/bin/env python3
"""Publish redacted local-ledger facts only; never invoke a transfer or provider."""
import argparse
import fcntl
import hashlib
import json
import os
import pathlib
import re
import stat
import subprocess
import sys
import tempfile

CONFIG_ROOT = pathlib.Path('/var/lib/firecontroller/operator-offsite-v1')
STAGING = pathlib.Path('/var/lib/firecontroller/operator-health-staging')
REPORTS = pathlib.Path('/var/lib/firecontroller/operator-health')
FIELDS = {'version', 'instance_id', 'observed_at', 'label', 'max_age_seconds', 'last_snapshot_at', 'last_attempt_at', 'attempt_state', 'attempt_reason', 'attempt_timeout_seconds', 'retention_warning'}
PROVIDERS = (('onedrive', 'OneDrive'), ('s3', 'S3'), ('google', 'Google Drive'))


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def regular(path, uid):
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == uid and info.st_gid == uid and stat.S_IMODE(info.st_mode) == 0o600, 'Unexpected private file metadata.')
    return info


def directory(path, uid):
    info = path.lstat()
    require(stat.S_ISDIR(info.st_mode) and not path.is_symlink() and info.st_uid == uid and info.st_gid == uid and stat.S_IMODE(info.st_mode) == 0o700, 'Unexpected private directory metadata.')


def mirror(path, uid, instance, label):
    require(regular(path, uid).st_size <= 8192, 'Oversized health mirror.')
    body = path.read_bytes()
    value = json.loads(body)
    require(isinstance(value, dict) and set(value) <= FIELDS and value.get('version') == 1 and value.get('instance_id') == instance and value.get('label') == label and value.get('max_age_seconds') == 10800, 'Unexpected mirror binding or disclosure.')
    return body


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def publish(binary, provider, label):
    config = CONFIG_ROOT / provider / 'offsite.json'
    require(regular(config, 0).st_size <= 16384, 'Unexpected canonical configuration.')
    settings = json.loads(config.read_bytes())
    instance = settings.get('instance_id', '')
    require(re.fullmatch(r'instance_[0-9a-f]{32}', instance) is not None, 'Unexpected instance identity.')
    require(settings.get('state_dir') == str(CONFIG_ROOT / provider / 'state'), 'Unexpected canonical ledger path.')
    stage, final = STAGING / (provider + '.json'), REPORTS / (provider + '.json')
    # Refuse an unrelated or predecessor-instance mirror instead of silently replacing it.
    if stage.exists() or stage.is_symlink():
        mirror(stage, 0, instance, label)
    if final.exists() or final.is_symlink():
        mirror(final, 65532, instance, label)
    completed = subprocess.run([str(binary), 'backup-offsite-status', '--config', str(config), '--health-file', str(stage), '--health-label', label, '--health-max-age', '3h'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15, check=False)
    require(completed.returncode == 0, 'Local status projection failed.')
    body = mirror(stage, 0, instance, label)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(prefix='.' + provider + '.health-', dir=REPORTS, delete=False) as output:
            temporary = pathlib.Path(output.name)
            os.fchmod(output.fileno(), 0o600)
            output.write(body)
            os.fchown(output.fileno(), 65532, 65532)
            output.flush()
            os.fsync(output.fileno())
        if final.exists() or final.is_symlink():
            mirror(final, 65532, instance, label)
        os.replace(temporary, final)
        temporary = None
        sync_directory(REPORTS)
    finally:
        if temporary is not None:
            temporary.unlink()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--fcd', required=True)
    parser.add_argument('--sha256', required=True)
    args = parser.parse_args()
    require(os.geteuid() == 0, 'Run as the selected operator.')
    binary = pathlib.Path(args.fcd)
    require(binary.is_absolute() and binary == binary.resolve() and re.fullmatch(r'[0-9a-f]{64}', args.sha256), 'Explicit immutable helper path and SHA256 required.')
    info = binary.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and info.st_mode & 0o111 and not info.st_mode & 0o022, 'Unexpected helper ownership or permissions.')
    checksum = hashlib.sha256()
    with binary.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            checksum.update(block)
    require(checksum.hexdigest() == args.sha256, 'Qualified helper hash mismatch.')
    directory(STAGING, 0)
    directory(REPORTS, 65532)
    lock_path = STAGING / 'publisher.lock'
    fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'rb+') as lock:
        regular(lock_path, 0)
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        failed = []
        for provider, label in PROVIDERS:
            try:
                publish(binary, provider, label)
            except Exception:
                # Never print raw status output, configuration, ledger or exception payloads.
                failed.append(label)
        print(json.dumps({'published': len(PROVIDERS) - len(failed), 'unavailable': failed}))
        return 1 if failed else 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as failure:
        reason = "qualified helper hash mismatch" if isinstance(failure, RuntimeError) and str(failure) == "Qualified helper hash mismatch." else "local health publication failed"
        print("Backup health publisher unavailable (" + reason + "); previous reports retained.", file=sys.stderr)
        raise SystemExit(1)
