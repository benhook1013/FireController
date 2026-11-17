#!/usr/bin/env python3
"""Bound schema-rehearsal disk output; never silently discard migrated state."""
from contextlib import ExitStack
import argparse
import fcntl
import json
import os
from pathlib import Path
import stat

import rehearsal_operation as operation
import failed_equivalent
import supervised_equivalent
from artifact_retention import Refuse, open_lease


def admit(root):
    """Small receipts may remain; one unresolved workspace blocks another copy."""
    root = Path(root)
    if not root.is_absolute() or root.resolve() != root:
        raise Refuse('canonical bounded rehearsal root required')
    info = root.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise Refuse('private bounded rehearsal root required')
    with os.scandir(root) as children:
        for index, child in enumerate(children):
            if index >= 1024:
                raise Refuse('rehearsal receipt budget reached')
            if not child.is_dir(follow_symlinks=False):
                raise Refuse('unknown bounded rehearsal entry')
            work = root / child.name
            if (work / supervised_equivalent.RECEIPT).exists():
                supervised_equivalent.verify_disposed(work)
                continue
            if (work / 'failed-equivalent-disposal.json').exists():
                failed_equivalent.verify_disposed(work)
                continue
            receipt = work / 'equivalent-output-disposal.json'
            if not receipt.exists() or (work / 'data' / 'firecontroller.sqlite').exists():
                raise Refuse('prior rehearsal output requires explicit disposition before another copy')
            s = receipt.lstat()
            if not stat.S_ISREG(s.st_mode) or s.st_size > 8192 or s.st_uid != os.geteuid() or s.st_mode & 0o077:
                raise Refuse('invalid rehearsal disposition receipt')
            body = json.loads(receipt.read_text())
            if body.get('state') != 'verified_equivalent_output_removed' or body.get('operation') != child.name:
                raise Refuse('unproven rehearsal disposition')
            allowed = {'started.json','terminal.json','rehearsal.json','data','tmp','equivalent-output-disposal.json'}
            if any(p.name not in allowed or p.is_symlink() for p in work.iterdir()):
                raise Refuse('unexpected retained rehearsal artifact')
            for name in ('data', 'tmp'):
                directory = work / name
                if directory.exists() and (not directory.is_dir() or any(directory.iterdir())):
                    raise Refuse('unresolved rehearsal directory')
    return root


def open_source_lease(path, uid):
    path = Path(path)
    if not path.is_absolute() or path.resolve() != path:
        raise Refuse('canonical source lease required')
    before = path.lstat()
    if not stat.S_ISREG(before.st_mode) or before.st_uid != uid or before.st_mode & 0o077:
        raise Refuse('unexpected source lease ownership or mode')
    fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
    opened = os.fstat(fd)
    if (before.st_dev, before.st_ino, before.st_uid, before.st_mode) != (opened.st_dev, opened.st_ino, opened.st_uid, opened.st_mode):
        os.close(fd)
        raise Refuse('source lease changed during open')
    return os.fdopen(fd, 'r+')


def execute(*, budget_lease, source_lease, source_lease_uid=None, discard_failed_equivalent=False, **args):
    if type(discard_failed_equivalent) is not bool:
        raise Refuse("explicit failure disposal mode required")
    if args.get('discard_passed_output'):
        raise Refuse('bounded producer forbids unverified automatic disposal')
    root = Path(args['root'])
    budget = Path(budget_lease)
    if budget == Path(args['lease']) or budget == root or root in budget.parents:
        raise Refuse('separate external budget lease required')
    source_lock = Path(source_lease)
    if source_lock in (budget, Path(args['lease']), root) or root in source_lock.parents:
        raise Refuse('separate external source lease required')
    with ExitStack() as locks:
        custody = locks.enter_context(open_lease(budget))
        fcntl.flock(custody, fcntl.LOCK_EX | fcntl.LOCK_NB)
        source_custody = locks.enter_context(open_source_lease(source_lock, os.geteuid() if source_lease_uid is None else source_lease_uid))
        fcntl.flock(source_custody, fcntl.LOCK_EX | fcntl.LOCK_NB)
        admit(root)
        try:
            result = operation.execute(**args)
        except operation.KnownForegroundFailure as failure:
            if discard_failed_equivalent:
                failed_equivalent.dispose(args, failure, [(budget,custody),(source_lock,source_custody)])
            raise  # Removing a redundant copy never turns a failed rehearsal into success.
        except operation.KnownSupervisedFailure as failure:
            if discard_failed_equivalent:
                supervised_equivalent.dispose(args,failure,[(budget,custody),(source_lock,source_custody)])
            raise
        work = root / args['operation']
        source, _ = operation.checked_file(args['source'], args['source_sha256'], args.get('max_database_bytes', 1024**3), args.get('source_owner_uid'))
        terminal = json.loads((work / 'terminal.json').read_text())
        output = work / 'data' / 'firecontroller.sqlite'
        output_sha, _ = operation.digest(output)
        if output_sha != terminal['output_sha256']:
            raise Refuse('rehearsal output changed after completion')
        if output_sha != args['source_sha256']:
            return dict(result, retention='explicit_disposition_required', retained_database_bytes=output.stat().st_size)
        # This invocation created the exact duplicate; retain the original and all proofs.
        # Reacquire its consumer lease before unlink, and never collect old work.
        with open_lease(args['lease']) as consumer:
            fcntl.flock(consumer, fcntl.LOCK_EX | fcntl.LOCK_NB)
            operation.checked_file(source, output_sha, args.get('max_database_bytes', 1024**3), args.get('source_owner_uid'))
            current, info = operation.checked_file(output, output_sha, args.get('max_database_bytes', 1024**3))
            if info.st_nlink != 1:
                raise Refuse('temporary output acquired another link')
            runner_copy = work / 'migration-runner'
            if runner_copy.exists():
                operation.checked_file(args['runner'], args['runner_sha256'], 1024**3)
                runner_copy, runner_info = operation.checked_file(runner_copy, args['runner_sha256'], 1024**3)
                if runner_info.st_nlink != 1:
                    raise Refuse('temporary runner acquired another link')
                runner_copy.unlink()
            current.unlink()
            fd = os.open(current.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
            operation.new_json(work / 'equivalent-output-disposal.json', {
                'state':'verified_equivalent_output_removed', 'operation':args['operation'],
                'sha256':output_sha, 'source':str(source), 'logical_bytes_removed':info.st_size})
        return dict(result, retention='verified_equivalent_output_removed')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ('budget-lease','source-lease','root','lease','source','source-sha256','runner','runner-sha256','revision','operation'):
        p.add_argument('--'+name, required=True)
    p.add_argument('--source-owner-uid', type=int)
    p.add_argument('--source-lease-uid', type=int)
    p.add_argument('--supervised', action='store_true')
    print(json.dumps(execute(**vars(p.parse_args())), sort_keys=True))


if __name__ == '__main__':
    main()
