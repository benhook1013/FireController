import hashlib
from pathlib import Path
import sqlite3
import subprocess
import sys
import json
import tempfile
import unittest
from unittest.mock import patch

import sqlite_storage as inventory


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / 'snapshot.sqlite'
        with sqlite3.connect(self.path) as db:
            db.execute('CREATE TABLE retained (id INTEGER PRIMARY KEY, private TEXT)')
            db.execute('CREATE INDEX by_private ON retained(private)')
            db.execute('INSERT INTO retained(private) VALUES (?)', ('secret-row-value',))
        self.before = hashlib.sha256(self.path.read_bytes()).hexdigest()

    def test_accounts_tables_indexes_without_values_or_writes(self):
        before_files = sorted(Path(self.temp.name).iterdir())
        report = inventory.inspect(self.path)
        self.assertEqual(report['state'], 'complete')
        self.assertEqual({o['name'] for o in report['objects']}, {'sqlite_schema', 'retained', 'by_private'})
        self.assertEqual(report['object_bytes'] + report['free_bytes'], report['pages'] * report['page_size'])
        self.assertNotIn('secret-row-value', str(report))
        self.assertEqual(hashlib.sha256(self.path.read_bytes()).hexdigest(), self.before)
        self.assertEqual(sorted(Path(self.temp.name).iterdir()), before_files)

    def test_sidecars_refuse_without_removal(self):
        for suffix in ('-wal', '-shm', '-journal'):
            sidecar = Path(str(self.path) + suffix)
            sidecar.write_bytes(b'retained')
            with self.assertRaises(inventory.Refuse):
                inventory.inspect(self.path)
            self.assertEqual(sidecar.read_bytes(), b'retained')
            sidecar.unlink()

    def test_symlink_refuses(self):
        link = self.path.parent / 'link'
        link.symlink_to(self.path)
        with self.assertRaises(inventory.Refuse):
            inventory.inspect(link)

    def test_object_budget_refuses_partial_success(self):
        with self.assertRaises(inventory.Refuse):
            inventory.inspect(self.path, max_objects=1)

    def test_deadline_refuses(self):
        with patch.object(inventory.time, 'monotonic', side_effect=[0, 10]):
            with self.assertRaises(inventory.Refuse):
                inventory.inspect(self.path, timeout=1)

    def test_change_refuses(self):
        original = inventory.identity
        calls = []
        def observe(path):
            result = original(path)
            calls.append(1)
            return result if len(calls) == 1 else (*result[:-1], result[-1] + 1)
        with patch.object(inventory, 'identity', side_effect=observe):
            with self.assertRaises(inventory.Refuse):
                inventory.inspect(self.path)

    def test_cli_reports_unavailable_without_private_path_or_sql(self):
        self.path.write_bytes(b'private-corrupt-content')
        run = subprocess.run([sys.executable, inventory.__file__, '--offline-snapshot', str(self.path)],
                             capture_output=True, text=True, timeout=5)
        self.assertEqual(run.returncode, 1)
        self.assertEqual(json.loads(run.stdout)['state'], 'unavailable')
        self.assertNotIn(str(self.path), run.stdout + run.stderr)
        self.assertNotIn('private-corrupt-content', run.stdout + run.stderr)
        self.assertEqual(run.stderr, '')

    def test_dbstat_shadow_cannot_expose_row_values(self):
        with sqlite3.connect(self.path) as db:
            db.execute('CREATE TABLE dbstat (name TEXT, pgsize INTEGER, payload INTEGER, unused INTEGER)')
            db.execute("INSERT INTO dbstat VALUES ('secret-row-value',1,1,1)")
        with self.assertRaises(sqlite3.Error):
            inventory.inspect(self.path)

    def test_dangling_sidecar_symlink_refuses(self):
        for suffix in ('-wal', '-shm', '-journal'):
            sidecar = Path(str(self.path) + suffix)
            sidecar.symlink_to(self.path.parent / 'missing')
            with self.assertRaises(inventory.Refuse):
                inventory.inspect(self.path)
            self.assertTrue(sidecar.is_symlink())
            sidecar.unlink()

    def test_indexes_attributed_to_tables_and_totals_not_double_counted(self):
        with sqlite3.connect(self.path) as db:
            db.execute('CREATE TABLE wide (id INTEGER PRIMARY KEY, payload TEXT UNIQUE)')
            db.executemany('INSERT INTO wide(payload) VALUES (?)', [(str(i) + 'x' * 1500,) for i in range(40)])
        before = hashlib.sha256(self.path.read_bytes()).hexdigest()
        report = inventory.inspect(self.path)
        owners = {r['table']: r for r in report['tables']}
        self.assertEqual(report['tables'][0]['table'], 'wide')
        self.assertGreater(owners['wide']['index_bytes'], 0)
        self.assertGreater(owners['wide']['table_bytes'], 0)
        self.assertEqual(sum(r['total_bytes'] for r in report['tables']) + report['unattributed_bytes'], report['object_bytes'])
        self.assertEqual(report['object_bytes'] + report['free_bytes'], report['pages'] * report['page_size'])
        self.assertTrue(any(r['name'].startswith('sqlite_autoindex_wide') and r['owner_table'] == 'wide' for r in report['objects']))
        self.assertEqual(hashlib.sha256(self.path.read_bytes()).hexdigest(), before)
        self.assertNotIn('x' * 1500, json.dumps(report))

    def test_without_rowid_and_free_pages_are_not_index_or_table_guesswork(self):
        with sqlite3.connect(self.path) as db:
            db.execute('CREATE TABLE compact (key TEXT PRIMARY KEY, value TEXT) WITHOUT ROWID')
            db.execute('CREATE INDEX compact_value ON compact(value)')
            db.executemany('INSERT INTO compact VALUES (?, ?)', [(str(i), 'v' * 1500) for i in range(60)])
            db.execute('DELETE FROM compact')
        report = inventory.inspect(self.path)
        owner = next(r for r in report['tables'] if r['table'] == 'compact')
        self.assertGreater(owner['index_bytes'], 0)
        self.assertGreater(owner['table_bytes'], 0)
        self.assertGreater(report['free_bytes'], 0)
        self.assertEqual(sum(r['total_bytes'] for r in report['tables']) + report['unattributed_bytes'] + report['free_bytes'], report['pages'] * report['page_size'])

    def test_same_named_trigger_does_not_duplicate_storage_rows(self):
        with sqlite3.connect(self.path) as db:
            db.execute('CREATE TRIGGER retained AFTER INSERT ON retained BEGIN SELECT 1; END')
        report = inventory.inspect(self.path)
        self.assertEqual(report['object_bytes'] + report['free_bytes'], report['pages'] * report['page_size'])
        self.assertEqual(report['unattributed_bytes'], 0)
        self.assertTrue(all(r['object_type'] in ('table', 'index') for r in report['objects']))
