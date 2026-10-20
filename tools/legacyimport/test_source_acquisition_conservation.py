# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
from pathlib import Path
import os
import tempfile
import unittest

from source_acquisition_conservation import ConservationRefusal, conserved_roots, inventory


class ConservationTests(unittest.TestCase):
    def setUp(self):
        self.owned = tempfile.TemporaryDirectory()
        self.addCleanup(self.owned.cleanup)
        self.root = Path(self.owned.name)
        self.file = self.root/'capture'
        self.file.write_bytes(b'original')

    def test_unchanged_inputs_are_conserved_and_body_not_exposed(self):
        with conserved_roots([self.root]) as proof:
            self.assertEqual(self.file.read_bytes(), b'original')
            self.assertNotIn('original', repr(proof))
        self.assertEqual(self.file.read_bytes(), b'original')

    def test_changed_added_removed_and_new_root_refuse(self):
        mutations = [lambda: self.file.write_bytes(b'changed'),
                     lambda: (self.root/'added').write_bytes(b'new'),
                     lambda: self.file.unlink()]
        for index, mutate in enumerate(mutations):
            folder=self.root/str(index);folder.mkdir()
            # Distinct owned roots avoid deleting or reverting prior mutations.
            original=self.file
            self.file=folder/'capture';self.file.write_bytes(b'original')
            with self.subTest(index=index), self.assertRaises(ConservationRefusal):
                with conserved_roots([folder]):
                    if index==1: (folder/'added').write_bytes(b'new')
                    else: mutate()
            self.file=original
        absent=self.root/'absent'
        with self.assertRaises(ConservationRefusal):
            with conserved_roots([absent]):
                absent.mkdir()

    def test_symlink_and_byte_entry_budget_refuse(self):
        with self.assertRaises(ConservationRefusal):
            inventory([self.root], max_bytes=2)
        with self.assertRaises(ConservationRefusal):
            inventory([self.root], max_entries=1)
        (self.root/'link').symlink_to(self.file)
        with self.assertRaises(ConservationRefusal):
            inventory([self.root])

    def test_exact_admitted_snapshot_keeps_hash_without_capture_byte_charge(self):
        snapshot = self.root/'snapshot.sqlite3'
        snapshot.write_bytes(b'separately admitted database')
        before = inventory([self.root])
        self.assertEqual(inventory([self.root], max_bytes=len(b'original'), admitted_snapshot=snapshot), before)
        entry = before['0/snapshot.sqlite3']
        self.assertEqual(entry[0], 'file')
        self.assertEqual(len(entry[2]), 64)
        with self.assertRaises(ConservationRefusal):
            inventory([self.root], max_bytes=len(b'original')-1, admitted_snapshot=snapshot)
        # A second path for the same inode is still an independently budgeted entry.
        os.link(snapshot, self.root/'alias')
        with self.assertRaises(ConservationRefusal):
            inventory([self.root], max_bytes=len(b'original'), admitted_snapshot=snapshot)

    def test_admitted_snapshot_retains_conservation_kind_and_entry_guards(self):
        snapshot = self.root/'snapshot.sqlite3'
        snapshot.write_bytes(b'separately admitted database')
        with self.assertRaises(ConservationRefusal):
            inventory([self.root], max_bytes=len(b'original'), max_entries=2, admitted_snapshot=snapshot)
        with self.assertRaises(ConservationRefusal):
            with conserved_roots([self.root], max_bytes=len(b'original'), admitted_snapshot=snapshot):
                snapshot.write_bytes(b'changed database')
        link = self.root/'snapshot-link'
        link.symlink_to(snapshot)
        with self.assertRaises(ConservationRefusal):
            inventory([self.root], admitted_snapshot=link)

    def test_failure_does_not_skip_final_conservation_check(self):
        with self.assertRaises(ConservationRefusal):
            with conserved_roots([self.root]):
                self.file.write_bytes(b'changed')
                raise RuntimeError('owned synthetic audit failure')


if __name__ == '__main__':
    unittest.main()
