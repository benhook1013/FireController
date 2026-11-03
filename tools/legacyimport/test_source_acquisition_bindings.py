# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Owned synthetic proof of private source-reader wiring; no live acquisition."""
from contextlib import closing
import fcntl
import importlib
from pathlib import Path
import socket
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from source_acquisition_bindings import BindingRefusal, bind_source

import os
SOURCE_ROOT = os.environ.get('FIRECONTROLLER_SOURCE_ROOT')
if not SOURCE_ROOT:
    raise unittest.SkipTest('Set FIRECONTROLLER_SOURCE_ROOT to an explicitly selected trusted source checkout')
SOURCE = Path(SOURCE_ROOT) / 'dev-tools'
sys.dont_write_bytecode = True
sys.path.insert(0, str(SOURCE))
HELPERS = {name: importlib.import_module('pr_review.' + name)
           for name in ('state', 'sqlite_store', 'sqlite_review_records', 'evidence', 'runtime', 'hosted')}


class WiringTests(unittest.TestCase):
    def setUp(self):
        self.owned = tempfile.TemporaryDirectory()
        self.addCleanup(self.owned.cleanup)
        self.common = Path(self.owned.name)
        self.logical = self.common / 'firemud' / 'pr-review-stack.json'
        self.logical.mkdir(parents=True, mode=0o700)
        self.database = self.logical.with_suffix('.sqlite3')
        state = HELPERS['state'].ReviewState(ordered_prs=(7,))
        HELPERS['sqlite_store'].SqliteStateStore(self.database).update(lambda _: state)
        HELPERS['sqlite_review_records'].SqliteReviewRecords(self.database).bootstrap()
        self.database.chmod(0o600)
        self.before = self.database.read_bytes()
        self.original_class = HELPERS['hosted'].SqliteReviewRecords
        self.original_common = HELPERS['evidence'].git_common_dir

    def assert_conserved(self):
        self.assertEqual(self.database.read_bytes(), self.before)
        self.assertFalse(any(Path(str(self.database) + suffix).exists()
                             for suffix in ('-wal', '-shm', '-journal')))
        self.assertIs(HELPERS['hosted'].SqliteReviewRecords, self.original_class)
        self.assertIs(HELPERS['evidence'].git_common_dir, self.original_common)

    def test_actual_readers_use_sealed_sqlite_and_deny_mutations(self):
        with patch.object(subprocess, 'run', side_effect=AssertionError('no subprocess')), \
                patch.object(subprocess, 'Popen', side_effect=AssertionError('no subprocess')), \
                patch.object(socket, 'socket', side_effect=AssertionError('no network')):
            with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
                self.assertEqual(tuple(store.load().ordered_prs), (7,))
                self.assertEqual(records.list_routes(status='open', target_pr=7, include_legacy_routes=True), [])
                self.assertEqual(HELPERS['evidence'].resolve_cli_capture_context(), (self.common, records))
                self.assertEqual(HELPERS['evidence'].resolve_cli_capture_context(self.common, records), (self.common, records))
                self.assertIsInstance(records, HELPERS['runtime'].SqliteReviewRecords)
                # Both Hosted constructors must use the same bound class.
                hosted_reader = HELPERS['hosted'].SqliteReviewRecords(self.database)
                self.assertEqual(hosted_reader.list_routes(status='open'), [])
                with closing(hosted_reader._connect(read_only=True)) as connection:
                    with self.assertRaises(sqlite3.OperationalError):
                        connection.execute('CREATE TABLE forbidden(x)')
                for mutate in (records.bootstrap, lambda: store.update(None), lambda: store.save(None)):
                    with self.assertRaises(BindingRefusal):
                        mutate()
        self.assert_conserved()

    def test_mismatched_capture_and_database_context_refuses(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (_, records):
            for callback in (
                lambda: HELPERS['evidence'].resolve_cli_capture_context(self.common / 'elsewhere'),
                lambda: HELPERS['evidence'].resolve_cli_capture_context(records=None),
                lambda: HELPERS['hosted'].SqliteReviewRecords(self.common / 'other.sqlite3'),
                lambda: HELPERS['evidence'].resolve_cli_capture_context(self.common, records, common=self.common),
            ):
                with self.assertRaises(BindingRefusal):
                    callback()
        self.assert_conserved()

    def test_sidecars_and_wrong_layout_refuse_before_binding(self):
        with self.assertRaises(BindingRefusal):
            with bind_source(HELPERS, common=self.common, snapshot=self.common / 'elsewhere.sqlite3'):
                self.fail('wrong database admitted')
        sidecar = Path(str(self.database) + '-wal')
        sidecar.write_bytes(b'owned synthetic unfinished WAL')
        try:
            with self.assertRaises(BindingRefusal):
                with bind_source(HELPERS, common=self.common, snapshot=self.database):
                    self.fail('unsealed database admitted')
        finally:
            sidecar.unlink()
        self.assert_conserved()

    def test_context_restored_after_audit_failure(self):
        with self.assertRaisesRegex(ValueError, 'audit refused'):
            with bind_source(HELPERS, common=self.common, snapshot=self.database):
                raise ValueError('audit refused')
        self.assert_conserved()

    def test_logical_facade_preserves_real_lock_observation(self):
        lock = self.common / 'firemud' / 'pr-review' / 'cli.lock'
        lock.parent.mkdir(mode=0o700)
        with lock.open('w') as held:
            fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
                evidence = HELPERS['runtime'].LiveEvidence('owner/project', object(), store, records)
                self.assertEqual(evidence.repository_cli_lock_status(), 'held')
            fcntl.flock(held, fcntl.LOCK_UN)
        self.assert_conserved()


if __name__ == '__main__':
    unittest.main()
