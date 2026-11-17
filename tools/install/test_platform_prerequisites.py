import hashlib
import os
from pathlib import Path
import tempfile
import unittest
import platform_prerequisites as p


class PlatformTests(unittest.TestCase):
    def test_complete_roles_remain_observations_not_execution(self):
        with tempfile.TemporaryDirectory() as root:
            binary = Path(root) / 'binary'
            binary.write_bytes(b'non-executed fixture')
            binary.chmod(0o555)
            digest = hashlib.sha256(binary.read_bytes()).hexdigest()
            report = p.inspect({role: {'path':str(binary), 'sha256':digest} for role in p.ROLES})
            self.assertEqual(len(report['roles']), 4)
            self.assertEqual(report['executables_run'], 0)
            self.assertFalse(report['generation_switch_ready'])
            self.assertFalse(report['platform_runtime_verified'])
            self.assertNotIn(root, str(report))

    def test_missing_unknown_and_digest_refused(self):
        for value in ({}, {'unknown':{}}, {role:{} for role in p.ROLES}):
            with self.assertRaises(p.Invalid): p.inspect(value)
        with self.assertRaises(p.Invalid): p.executable_pin('/unused', 'bad')

    def test_link_fifo_writable_nonexecutable_and_wrong_bytes_refused(self):
        with tempfile.TemporaryDirectory() as root:
            binary = Path(root) / 'binary'
            binary.write_bytes(b'fixture')
            digest = hashlib.sha256(b'fixture').hexdigest()
            for mode in (0o644, 0o777):
                binary.chmod(mode)
                with self.assertRaises(p.Invalid): p.executable_pin(str(binary), digest)
            binary.chmod(0o555)
            with self.assertRaises(p.Invalid): p.executable_pin(str(binary), '0'*64)
            link = Path(root) / 'link'
            link.symlink_to(binary)
            with self.assertRaises(p.Invalid): p.executable_pin(str(link), digest)
            fifo = Path(root) / 'fifo'
            os.mkfifo(fifo)
            with self.assertRaises(p.Invalid): p.executable_pin(str(fifo), digest)

    def test_ancestor_symlink_refused(self):
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root) / 'directory'
            directory.mkdir()
            binary = directory / 'binary'
            binary.write_bytes(b'fixture')
            binary.chmod(0o555)
            (Path(root) / 'alias').symlink_to(directory, target_is_directory=True)
            with self.assertRaises((p.Invalid, OSError)):
                p.executable_pin(str(Path(root) / 'alias/binary'), hashlib.sha256(b'fixture').hexdigest())
