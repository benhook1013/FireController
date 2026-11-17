#!/usr/bin/env python3
"""Bounded storage accounting for an existing, quiescent SQLite snapshot only."""
import argparse
import json
from pathlib import Path
import sqlite3
import stat
import time


class Refuse(ValueError):
    pass


def identity(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or path.resolve() != path:
        raise Refuse('canonical regular snapshot required')
    if any(p.exists() or p.is_symlink() for p in (Path(str(path) + suffix) for suffix in ('-wal', '-shm', '-journal'))):
        raise Refuse('snapshot has live or unresolved SQLite sidecars')
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def inspect(path, *, timeout=5.0, max_objects=1024):
    if not 0 < timeout <= 30 or type(max_objects) is not int or not 1 <= max_objects <= 1024:
        raise Refuse('invalid inspection budget')
    path = Path(path)
    before = identity(path)
    started = time.monotonic()
    # Immutable is correct only for the caller's existing offline snapshot.
    # It prevents SQLite creating/writing SHM or journals; never use on the live DB.
    connection = sqlite3.connect(path.as_uri() + '?mode=ro&immutable=1', uri=True)
    try:
        connection.execute('PRAGMA query_only=ON')
        connection.execute('PRAGMA temp_store=MEMORY')
        connection.set_progress_handler(lambda: time.monotonic() - started >= timeout, 1000)
        page_size = connection.execute('PRAGMA page_size').fetchone()[0]
        pages = connection.execute('PRAGMA page_count').fetchone()[0]
        free = connection.execute('PRAGMA freelist_count').fetchone()[0]
        rows = connection.execute('''SELECT name, COUNT(*), SUM(pgsize), SUM(payload), SUM(unused)
            FROM dbstat('main') GROUP BY name ORDER BY name LIMIT ?''', (max_objects + 1,)).fetchall()
        if len(rows) > max_objects:
            raise Refuse('snapshot object budget exceeded')
        if time.monotonic() - started >= timeout:
            raise Refuse('snapshot inspection deadline exceeded')
        if identity(path) != before:
            raise Refuse('snapshot changed during inspection')
        objects = [dict(name=r[0], pages=r[1], allocated_bytes=r[2], payload_bytes=r[3], unused_bytes=r[4]) for r in rows]
        return dict(state='complete', source_kind='offline_snapshot', file_bytes=before[2], page_size=page_size,
                    pages=pages, free_pages=free, free_bytes=free * page_size, objects=objects,
                    object_bytes=sum(r['allocated_bytes'] for r in objects),
                    note='Unused page bytes are not a safe deletion or VACUUM recommendation.')
    finally:
        connection.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--offline-snapshot', type=Path, required=True)
    parser.add_argument('--timeout', type=float, default=5.0)
    args = parser.parse_args()
    try:
        result = inspect(args.offline_snapshot, timeout=args.timeout)
    except (Refuse, OSError, sqlite3.Error, ValueError):
        # Never emit filesystem paths, row values or raw database/provider errors.
        print(json.dumps(dict(state='unavailable', reason='snapshot_or_inspection_unqualified')))
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
