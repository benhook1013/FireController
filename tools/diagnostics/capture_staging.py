#!/usr/bin/env python3
"""Read-only bounded inventory of capture staging entries; never declares orphans."""
import argparse
import json
import os
import re
from pathlib import Path
import stat
import time

PREFIXES = ('.capture-', '.hosted-capture-', '.exhaustion-')
FINAL_NAME = re.compile(r'[A-Za-z0-9_-]{1,128}\.json\Z')
HOSTED_NAME = re.compile(r'[A-Za-z0-9_-]{1,128}-hosted-[0-9a-f]{64}\.json\Z')


def inspect(root, *, max_entries=4096, timeout=5.0):
    if type(max_entries) is not int or not 1 <= max_entries <= 65536 or not 0 < timeout <= 30:
        raise ValueError('invalid budget')
    root = Path(root)
    if not root.is_absolute() or root.resolve() != root:
        raise ValueError('canonical absolute directory required')
    # Open the directory itself, not files inside it; never follow staging links.
    fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        before = os.fstat(fd)
        deadline = time.monotonic() + timeout
        counts = {p: dict(files=0, logical_bytes=0, allocated_bytes=0, other_entries=0, with_published_name_hardlink=0, without_published_name_hardlink=0) for p in PREFIXES}
        visited = 0
        published = set()
        staged = []
        with os.scandir(fd) as entries:
            for entry in entries:
                visited += 1
                if visited > max_entries or time.monotonic() >= deadline:
                    raise ValueError('inventory budget exceeded')
                prefix = next((p for p in PREFIXES if entry.name.startswith(p)), None)
                is_final = FINAL_NAME.fullmatch(entry.name) or HOSTED_NAME.fullmatch(entry.name)
                if prefix is None and not is_final:
                    continue
                info = entry.stat(follow_symlinks=False)
                inode = (info.st_dev, info.st_ino)
                if prefix is None:
                    if stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600:
                        published.add(inode)
                    continue
                item = counts[prefix]
                if not stat.S_ISREG(info.st_mode):
                    item['other_entries'] += 1
                    continue
                staged.append((prefix, inode))
                item['files'] += 1
                item['logical_bytes'] += info.st_size
                item['allocated_bytes'] += info.st_blocks * 512
        for prefix, inode in staged:
            key = 'with_published_name_hardlink' if inode in published else 'without_published_name_hardlink'
            counts[prefix][key] += 1
        after = os.fstat(fd)
        if (before.st_mtime_ns, before.st_ctime_ns) != (after.st_mtime_ns, after.st_ctime_ns):
            raise ValueError('directory changed during inventory')
        if time.monotonic() >= deadline:
            raise ValueError('inventory deadline exceeded')
        return dict(state='observed', entries_scanned=visited, staging=counts,
                    note='Entries may be active or retained crash evidence; none are deletion candidates. Published-name links are metadata observations, not validated captures or writer custody. Allocated bytes are summed references, not unique reclaimable space.')
    finally:
        os.close(fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--capture-root', type=Path, required=True)
    args = parser.parse_args()
    try:
        result = inspect(args.capture_root)
    except (OSError, ValueError):
        print(json.dumps(dict(state='unavailable', reason='inventory_incomplete')))
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
