# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
from pathlib import Path
import os
import tempfile
import unittest

from source_acquisition_conservation import (ConservationRefusal, conserved_roots, inventory,
    DEFAULT_CAPTURE_BYTES, DEFAULT_CAPTURE_ENTRIES, MAX_CAPTURE_BYTES, MAX_CAPTURE_ENTRIES)


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

    def test_exact_byte_and_entry_boundaries_preserve_default_manifest(self):
        self.assertEqual((DEFAULT_CAPTURE_BYTES, DEFAULT_CAPTURE_ENTRIES), (128 << 20, 20000))
        default = inventory([self.root])
        self.assertEqual(inventory([self.root], max_bytes=8, max_entries=2), default)
        self.assertEqual(inventory([self.root], max_bytes=MAX_CAPTURE_BYTES,
                                   max_entries=MAX_CAPTURE_ENTRIES), default)
        for limits in (dict(max_bytes=7, max_entries=2), dict(max_bytes=8, max_entries=1)):
            with self.subTest(limits=limits), self.assertRaises(ConservationRefusal) as failure:
                inventory([self.root], **limits)
            self.assertEqual(failure.exception.args, ('capture_inventory_too_large',))

    def test_invalid_caps_refuse_before_visiting_inputs(self):
        def forbidden_roots():
            self.fail('invalid capture cap visited filesystem inputs')
            yield self.root
        for name, maximum in (('max_bytes', MAX_CAPTURE_BYTES), ('max_entries', MAX_CAPTURE_ENTRIES)):
            for value in (0, -1, True, False, None, '1', 1.5, maximum+1):
                with self.subTest(name=name, value=value), self.assertRaises(ConservationRefusal) as failure:
                    inventory(forbidden_roots(), **{name: value})
                self.assertEqual(failure.exception.args, ('capture_limits_invalid',))

    def test_final_pass_uses_selected_caps_after_body_failure(self):
        for name in ('bytes', 'entries'):
            root = self.root/name
            root.mkdir()
            capture = root/'capture'
            capture.write_bytes(b'original')
            with self.subTest(name=name), self.assertRaises(ConservationRefusal) as failure:
                with conserved_roots([root], max_bytes=8, max_entries=2):
                    if name == 'bytes':
                        capture.write_bytes(b'original+')
                    else:
                        (root/'added').write_bytes(b'')
                    raise RuntimeError('owned synthetic body failure')
            self.assertEqual(failure.exception.args, ('capture_inventory_too_large',))

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
        charged = len(b'original')+snapshot.stat().st_size
        manifest = inventory([self.root], max_bytes=charged, max_entries=4, admitted_snapshot=snapshot)
        self.assertEqual(manifest['0/alias'][2], manifest['0/snapshot.sqlite3'][2])
        for limits in (dict(max_bytes=charged-1, max_entries=4), dict(max_bytes=charged, max_entries=3)):
            with self.subTest(limits=limits), self.assertRaises(ConservationRefusal):
                inventory([self.root], admitted_snapshot=snapshot, **limits)

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
