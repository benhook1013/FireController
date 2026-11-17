# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Source-owner wiring for a sealed, canonical-layout handoff.

This is one acquisition component, not an exporter or a quiescence assertion.
Run in a dedicated process: the selected trusted source has module-level context.
No installed controller, database or capture directory is discovered here.
"""
from contextlib import contextmanager
from pathlib import Path
import os
import sqlite3
import stat
import sys


class BindingRefusal(Exception):
    pass


def exact_path(value):
    path = Path(value)
    if not path.is_absolute() or path.resolve() != path:
        raise BindingRefusal("source_path_not_explicit")
    return path


def sealed_database(path):
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077
            or info.st_uid != os.getuid()):
        raise BindingRefusal("source_database_not_private_regular")
    for suffix in ("-wal", "-shm", "-journal"):
        if Path(str(path) + suffix).exists() or Path(str(path) + suffix).is_symlink():
            raise BindingRefusal("source_database_not_sealed")


def immutable_connection(path, timeout):
    sealed_database(path)
    connection = sqlite3.connect(path.as_uri() + "?mode=ro&immutable=1", uri=True,
                                 timeout=timeout, isolation_level=None)
    try:
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA query_only = ON")
        return connection
    except BaseException:
        connection.close()
        raise


@contextmanager
def bind_source(helpers, *, common, snapshot):
    """Bind known source readers; caller still owns pause/conservation/audit.

    Only a private canonical-layout sealed handoff is supported. In particular,
    this does not redirect captures from one source to a different database.
    Git/provider construction and use remain the caller's separate obligation.
    """
    common, snapshot = exact_path(common), exact_path(snapshot)
    logical = common / "firemud" / "pr-review-stack.json"
    if (not common.is_dir() or not logical.is_dir() or logical.is_symlink()
            or common.stat().st_mode & 0o077 or logical.stat().st_mode & 0o077
            or snapshot != logical.with_suffix(".sqlite3")):
        raise BindingRefusal("source_layout_mismatch")
    sealed_database(snapshot)
    evidence = helpers["evidence"]
    source_records = helpers["sqlite_review_records"].SqliteReviewRecords
    state_type = helpers["sqlite_store"].SqliteStateStore

    class ReadRecords(source_records):
        def __init__(self, path, **kwargs):
            if exact_path(path) != snapshot:
                raise BindingRefusal("source_database_mismatch")
            super().__init__(snapshot, **kwargs)

        def _connect(self, *, read_only, deadline=None):
            if read_only is not True:
                raise BindingRefusal("source_write_forbidden")
            timeout = self._remaining_timeout(self._effective_deadline(deadline))
            return immutable_connection(snapshot, timeout)

    class ReadState(state_type):
        def _connect_read_only(self, *, deadline=None):
            return immutable_connection(snapshot, self._remaining_timeout(deadline))

        def update(self, *args, **kwargs):
            raise BindingRefusal("source_write_forbidden")

    state_reader = ReadState(snapshot)
    records = ReadRecords(snapshot)

    class LogicalState:
        path = logical

        def load(self, *, deadline=None):
            return state_reader.load(deadline=deadline)

        def save(self, *args, **kwargs):
            raise BindingRefusal("source_write_forbidden")

        update = save

    original_context = evidence.resolve_cli_capture_context

    def context(*args, **kwargs):
        if len(args) > 2 or set(kwargs) - {"common", "records"}:
            raise BindingRefusal("source_context_unsupported")
        if (args and "common" in kwargs) or (len(args) > 1 and "records" in kwargs):
            raise BindingRefusal("source_context_unsupported")
        selected_common = args[0] if args else kwargs.get("common")
        selected_records = args[1] if len(args) > 1 else kwargs.get("records", records)
        if selected_common is not None and exact_path(selected_common) != common:
            raise BindingRefusal("source_capture_mismatch")
        if selected_records is not records:
            raise BindingRefusal("source_records_mismatch")
        return original_context(common, records=records)

    changes = []

    def replace(owner, name, value):
        changes.append((owner, name, getattr(owner, name)))
        setattr(owner, name, value)

    try:
        # Imported class aliases in Hosted must obey the same immutable binding.
        # Preserve a class rather than a factory: source code uses isinstance.
        for name, module in list(sys.modules.items()):
            if name == "pr_review" or name.startswith("pr_review."):
                if getattr(module, "SqliteReviewRecords", None) is source_records:
                    replace(module, "SqliteReviewRecords", ReadRecords)
        replace(evidence, "git_common_dir", lambda: common)
        replace(evidence, "resolve_cli_capture_context", context)
        yield LogicalState(), records
    finally:
        for owner, name, value in reversed(changes):
            setattr(owner, name, value)
