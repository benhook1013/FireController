"""Separate image preparation from guarded admission for fresh operations.

Isolated candidate, not a production launcher. The caller supplies reviewed
source verification, image production and the unchanged rollout/window checks.
Only the release lease is held here; build must retain its artifact lease.
No backup lease is acquired during preparation. Private cooperating tree only.
"""
import contextlib
import fcntl
import hashlib
import json
import os
import stat
from pathlib import Path


def validate_custody(fd, path):
    a, b = os.fstat(fd), os.stat(path, follow_symlinks=False)
    if not stat.S_ISREG(b.st_mode) or (a.st_dev, a.st_ino) != (b.st_dev, b.st_ino):
        raise ValueError('release lease replaced')


@contextlib.contextmanager
def custody(path):
    fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        a, b = os.fstat(fd), os.stat(path, follow_symlinks=False)
        if (a.st_dev, a.st_ino) != (b.st_dev, b.st_ino):
            raise ValueError('release lease replaced')
        yield fd
        b = os.stat(path, follow_symlinks=False)
        if (a.st_dev, a.st_ino) != (b.st_dev, b.st_ino):
            raise ValueError('release lease replaced')
    finally:
        os.close(fd)


def durable_new(path, value):
    raw = json.dumps(value, sort_keys=True, separators=(',', ':')).encode()
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as out:
        out.write(raw)
        out.flush()
        os.fsync(out.fileno())
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def manifest_hash(root):
    path = root / 'image-manifest.json'
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as src:
        raw = src.read(65537)
    if not raw or len(raw) > 65536:
        raise ValueError('invalid image manifest size')
    return hashlib.sha256(raw).hexdigest()


def read_receipt(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as src:
        raw = src.read(65537)
    if len(raw) > 65536:
        raise ValueError('receipt too large')
    return json.loads(raw)


def prepare(root, release_lease, identity, verify, build):
    """One producer invocation. Any interruption fences this operation forever.

    verify(fd) must verify exact reviewed inputs and return current preimage.
    build(fd) must own artifact custody, reap its tree and validate image output.
    No window check: preparation cannot drain, replace, or hold backup leases.
    """
    root = Path(root)
    with custody(release_lease) as fd:
        before = verify(fd)
        validate_custody(fd, release_lease)
        if (root / 'admission-started.json').exists():
            raise ValueError('admission already started')
        durable_new(root / 'preparation-started.json', {'identity': identity})
        validate_custody(fd, release_lease)
        build(fd)
        if verify(fd) != before:
            raise ValueError('preimage changed during preparation')
        receipt = {'identity': identity, 'preimage': before,
                   'manifest_sha256': manifest_hash(root)}
        validate_custody(fd, release_lease)
        durable_new(root / 'prepared-image.json', receipt)
        return receipt


def admit(root, release_lease, identity, verify, window, rollout):
    """Fresh custody, exact prepared output and unchanged window before launch.

    A failed window check leaves preparation reusable; it cannot start rollout.
    Once admission-started is durable, no replay, even if rollout raises.
    rollout(fd) must retain all existing post-acquisition window/preimage guards.
    """
    root = Path(root)
    with custody(release_lease) as fd:
        receipt = read_receipt(root / 'prepared-image.json')
        if set(receipt) != {'identity', 'preimage', 'manifest_sha256'}:
            raise ValueError('invalid prepared receipt')
        if receipt['identity'] != identity or receipt['manifest_sha256'] != manifest_hash(root):
            raise ValueError('prepared identity/output changed')
        if verify(fd) != receipt['preimage']:
            raise ValueError('current preimage changed')
        window()
        # Recheck after the potentially slow window observation, under custody.
        if verify(fd) != receipt['preimage'] or manifest_hash(root) != receipt['manifest_sha256']:
            raise ValueError('admission observation changed')
        validate_custody(fd, release_lease)
        durable_new(root / 'admission-started.json', {'identity': identity})
        validate_custody(fd, release_lease)
        return rollout(fd)
