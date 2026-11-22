import hashlib
import io
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import deploy_helper_package
import generation_package


SOURCE = 'e' * 40
IMAGE = 'ghcr.io/example/firecontroller@sha256:' + 'a' * 64
MEMBERS = {'manifest.json', 'fcd', 'rclone', 'deploy-helpers.tar'}


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def elf(machine, payload):
    identity = b'\x7fELF\x02\x01\x01' + bytes(9)
    header = struct.pack('<16sHHIQQQIHHHHHH', identity, 2, machine, 1,
                         0, 0, 0, 0, 64, 56, 0, 0, 0, 0)
    return header + payload


class GenerationPackageTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.fcd = self.root / 'fcd'
        self.rclone = self.root / 'rclone'
        self.output = self.root / 'generation.tar'
        for path in (self.fcd, self.rclone):
            path.write_bytes(elf(62, path.name.encode('ascii')))
            path.chmod(0o555)
        sources = {module: b'import os\n' for module in deploy_helper_package.module_inventory(SOURCE)}
        with patch.object(deploy_helper_package, 'source_at', return_value=sources):
            self.helpers = deploy_helper_package.assemble(self.root, SOURCE)
        extractor = patch.object(generation_package, '_extract_fcd', side_effect=self.extract_fcd)
        self.extractor = extractor.start()
        self.addCleanup(extractor.stop)

    def extract_fcd(self, image, source, output):
        self.assertEqual((image, source), (IMAGE, SOURCE))
        output = Path(output)
        output.write_bytes(self.fcd.read_bytes())
        output.chmod(0o555)

    def assemble(self, output=None, *, fcd_sha256=None):
        with patch.object(deploy_helper_package, 'assemble', return_value=self.helpers) as nested:
            receipt = generation_package.assemble(
                self.root, SOURCE, IMAGE, fcd_sha256 or digest(self.fcd),
                self.rclone, digest(self.rclone), output or self.output)
        nested.assert_called_once()
        return receipt

    def assert_receipt(self, receipt, archive):
        self.assertEqual(receipt['source'], SOURCE)
        self.assertEqual(receipt['image'], IMAGE)
        self.assertEqual(receipt['package_sha256'], digest(archive))
        self.assertIs(receipt['installation_ready'], False)

    def test_deterministic_immutable_round_trip(self):
        second = self.root / 'second.tar'
        self.assert_receipt(self.assemble(), self.output)
        self.assert_receipt(self.assemble(second), second)
        self.assertEqual(self.output.read_bytes(), second.read_bytes())
        self.assertEqual(self.output.stat().st_mode & 0o222, 0)
        with tarfile.open(self.output) as archive:
            self.assertEqual(len(archive.getmembers()), len(MEMBERS))
            self.assertEqual(set(archive.getnames()), MEMBERS)
            self.assertTrue(all(member.isfile() for member in archive.getmembers()))
            with archive.extractfile('fcd') as daemon:
                self.assertEqual(daemon.read(), self.fcd.read_bytes())
        receipt = generation_package.verify(self.output, digest(self.output), SOURCE, IMAGE)
        self.assert_receipt(receipt, self.output)

    def test_wrong_external_hash_or_generation_is_rejected(self):
        self.assemble()
        cases = (
            ('0' * 64, SOURCE, IMAGE),
            (digest(self.output), 'f' * 40, IMAGE),
            (digest(self.output), SOURCE, IMAGE[:-1] + 'b'),
        )
        for expected_hash, expected_source, expected_image in cases:
            with self.subTest(expected_hash=expected_hash, expected_source=expected_source,
                              expected_image=expected_image):
                with self.assertRaises(generation_package.Invalid):
                    generation_package.verify(self.output, expected_hash, expected_source, expected_image)

    def test_reselected_outer_hash_does_not_hide_member_damage(self):
        self.assemble()
        for change in ('missing', 'tampered'):
            with self.subTest(change=change):
                damaged = self.root / f'{change}.tar'
                with tarfile.open(self.output) as original, tarfile.open(damaged, 'w') as altered:
                    for member in original:
                        if member.name == 'rclone' and change == 'missing':
                            continue
                        if member.name == 'fcd' and change == 'tampered':
                            altered.addfile(member, io.BytesIO(elf(62, b'bad')))
                        else:
                            with original.extractfile(member) as body:
                                altered.addfile(member, body)
                damaged.chmod(0o444)
                with self.assertRaises(generation_package.Invalid):
                    generation_package.verify(damaged, digest(damaged), SOURCE, IMAGE)

    def test_unsupported_executable_architecture_is_rejected(self):
        self.rclone.chmod(0o755)
        self.rclone.write_bytes(elf(183, b'rclone'))
        self.rclone.chmod(0o555)
        with self.assertRaises(generation_package.Invalid):
            self.assemble()
        self.assertFalse(self.output.exists())

    def test_existing_destination_is_preserved(self):
        self.output.write_bytes(b'previous generation')
        self.output.chmod(0o444)
        original = self.output.stat()
        with self.assertRaises(generation_package.Invalid):
            self.assemble()
        self.assertEqual(self.output.read_bytes(), b'previous generation')
        self.assertEqual(self.output.stat().st_ino, original.st_ino)

    def test_failed_assembly_does_not_publish_a_file(self):
        with self.assertRaises(generation_package.Invalid):
            self.assemble(fcd_sha256='0' * 64)
        self.assertFalse(self.output.exists())

    def test_pin_for_other_image_daemon_does_not_publish_a_file(self):
        other_image_sha256 = hashlib.sha256(elf(62, b'other image daemon')).hexdigest()
        self.assertNotEqual(other_image_sha256, digest(self.fcd))
        with self.assertRaises(generation_package.Invalid):
            self.assemble(fcd_sha256=other_image_sha256)
        self.extractor.assert_called_once()
        self.assertFalse(self.output.exists())
