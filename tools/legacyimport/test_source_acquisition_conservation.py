# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
from pathlib import Path
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

    def test_failure_does_not_skip_final_conservation_check(self):
        with self.assertRaises(ConservationRefusal):
            with conserved_roots([self.root]):
                self.file.write_bytes(b'changed')
                raise RuntimeError('owned synthetic audit failure')


if __name__ == '__main__':
    unittest.main()
