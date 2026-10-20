# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Bounded read-only capture inventory; observed conservation, not a fence."""
from contextlib import contextmanager
import hashlib
import os
from pathlib import Path
import stat
import time


class ConservationRefusal(Exception):
    pass


def identity(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid,
            info.st_size, info.st_mtime_ns, info.st_nlink)


def inventory(roots, *, max_bytes=128 << 20, max_entries=20000, timeout=15, admitted_snapshot=None):
    deadline, remaining_bytes, remaining_entries = time.monotonic()+timeout, max_bytes, max_entries
    result = {}
    if admitted_snapshot is not None:
        admitted_snapshot = Path(admitted_snapshot)
        if not admitted_snapshot.is_absolute() or admitted_snapshot.resolve() != admitted_snapshot:
            raise ConservationRefusal('capture_root_not_explicit')

    def budget():
        if time.monotonic() > deadline:
            raise ConservationRefusal('capture_inventory_timed_out')

    def visit(path, label, depth):
        nonlocal remaining_bytes, remaining_entries
        budget()
        remaining_entries -= 1
        if remaining_entries < 0 or depth > 64:
            raise ConservationRefusal('capture_inventory_too_large')
        info = path.lstat()
        if info.st_uid != os.getuid():
            raise ConservationRefusal('capture_owner_mismatch')
        if stat.S_ISDIR(info.st_mode):
            result[label] = ('directory', identity(info))
            with os.scandir(path) as entries:
                names = []
                for entry in entries:
                    budget()
                    names.append(entry.name)
                    if len(names) > remaining_entries:
                        raise ConservationRefusal('capture_inventory_too_large')
            for name in sorted(names):
                visit(path/name, label+'/'+name, depth+1)
            if identity(path.lstat()) != identity(info):
                raise ConservationRefusal('capture_changed')
        elif stat.S_ISREG(info.st_mode):
            # The exact snapshot has its own open-FD byte admission and final
            # rehash. Keep its full manifest evidence without charging it to
            # the independent capture budget. Aliases/other files still count.
            if path != admitted_snapshot:
                remaining_bytes -= info.st_size
            if remaining_bytes < 0:
                raise ConservationRefusal('capture_inventory_too_large')
            digest, size = hashlib.sha256(), 0
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
            with os.fdopen(fd, 'rb') as handle:
                if identity(os.fstat(handle.fileno())) != identity(info):
                    raise ConservationRefusal('capture_changed')
                while block := handle.read(1 << 20):
                    budget()
                    size += len(block)
                    if size > info.st_size:
                        raise ConservationRefusal('capture_changed')
                    digest.update(block)
                if size != info.st_size or identity(path.lstat()) != identity(info):
                    raise ConservationRefusal('capture_changed')
            result[label] = ('file', identity(info), digest.hexdigest())
        else:
            raise ConservationRefusal('capture_kind_unsupported')

    for index, root in enumerate(roots):
        root = Path(root)
        if not root.is_absolute() or root.resolve() != root:
            raise ConservationRefusal('capture_root_not_explicit')
        if os.path.lexists(root):
            visit(root, str(index), 0)
        else:
            result[str(index)] = ('absent',)
    return result


@contextmanager
def conserved_roots(roots, **limits):
    before = inventory(roots, **limits)
    try:
        yield before
    finally:
        if inventory(roots, **limits) != before:
            raise ConservationRefusal('capture_changed')
