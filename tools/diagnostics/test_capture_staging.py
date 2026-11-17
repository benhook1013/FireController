import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import capture_staging as inventory


class StagingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def test_metadata_only_and_no_symlink_follow(self):
        staged = self.root / '.capture-secret-name'
        staged.write_bytes(b'private capture')
        (self.root / 'published.json').write_bytes(b'kept')
        (self.root / '.hosted-capture-link').symlink_to(staged)
        (self.root / '.exhaustion-directory').mkdir()
        result = inventory.inspect(self.root)
        self.assertEqual(result['staging']['.capture-']['files'], 1)
        self.assertEqual(result['staging']['.capture-']['allocated_bytes'], staged.stat().st_blocks * 512)
        self.assertEqual(result['staging']['.hosted-capture-']['other_entries'], 1)
        self.assertNotIn('secret-name', json.dumps(result))
        self.assertNotIn('private capture', json.dumps(result))
        self.assertEqual(staged.read_bytes(), b'private capture')

    def test_budget_counts_unrelated_files_too(self):
        for name in ('one', 'two'):
            (self.root / name).touch()
        with self.assertRaises(ValueError):
            inventory.inspect(self.root, max_entries=1)

    def test_root_link_refused(self):
        link = self.root / 'link'
        link.symlink_to(self.root)
        with self.assertRaises(ValueError):
            inventory.inspect(link)

    def test_deadline_refuses_empty_directory(self):
        with patch.object(inventory.time, 'monotonic', side_effect=[0, 10]):
            with self.assertRaises(ValueError):
                inventory.inspect(self.root)

    def test_abrupt_exit_leaves_staging_and_inventory_preserves_it(self):
        # Exercise OS process-death semantics used by Go's defer cleanup too.
        run = subprocess.run([sys.executable, '-c', 'import os,tempfile; f=tempfile.NamedTemporaryFile(prefix=".capture-",dir=os.environ["ROOT"]); f.write(b"retained"); f.flush(); os._exit(7)'],
                             env={**os.environ, 'ROOT': str(self.root)}, timeout=5)
        self.assertEqual(run.returncode, 7)
        result = inventory.inspect(self.root)
        self.assertEqual(result['staging']['.capture-']['files'], 1)
        self.assertEqual(result['staging']['.capture-']['logical_bytes'], 8)
        self.assertEqual(len(list(self.root.iterdir())), 1)

    def test_link_classification_never_assumes_equal_contents_means_same_inode(self):
        temp = self.root / '.capture-stage'
        temp.write_bytes(b'capture')
        temp.chmod(0o600)
        other = self.root / 'attempt.json'
        other.write_bytes(b'capture')
        other.chmod(0o600)
        report = inventory.inspect(self.root)['staging']['.capture-']
        self.assertEqual(report['without_published_name_hardlink'], 1)
        other.unlink()
        os.link(temp, other)
        report = inventory.inspect(self.root)['staging']['.capture-']
        self.assertEqual(report['with_published_name_hardlink'], 1)
        self.assertEqual(temp.read_bytes(), b'capture')

    def test_arbitrary_name_and_symlink_are_not_published_links(self):
        temp = self.root / '.exhaustion-stage'
        temp.write_bytes(b'capture')
        temp.chmod(0o600)
        os.link(temp, self.root / 'arbitrary')
        (self.root / 'attempt.json').symlink_to(temp)
        report = inventory.inspect(self.root)['staging']['.exhaustion-']
        self.assertEqual(report['without_published_name_hardlink'], 1)

    def test_hosted_published_name_link(self):
        temp = self.root / '.hosted-capture-stage'
        temp.write_bytes(b'capture')
        temp.chmod(0o600)
        os.link(temp, self.root / ('a' * 128 + '-hosted-' + 'f' * 64 + '.json'))
        report = inventory.inspect(self.root)['staging']['.hosted-capture-']
        self.assertEqual(report['with_published_name_hardlink'], 1)

    def test_world_readable_link_does_not_qualify(self):
        temp = self.root / '.capture-stage'
        temp.write_bytes(b'capture')
        temp.chmod(0o644)
        os.link(temp, self.root / 'attempt.json')
        report = inventory.inspect(self.root)['staging']['.capture-']
        self.assertEqual(report['without_published_name_hardlink'], 1)
