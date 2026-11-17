import hashlib
import io
import json
import os
import pathlib
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import package_bundle as b

class BundleTest(unittest.TestCase):
    def fixture(self,root):
        helper=root/'reader';helper.write_bytes(b'synthetic reader');return helper
    def test_reproducible_exact_members_no_execution(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=pathlib.Path(tmp);helper=self.fixture(root);pin=hashlib.sha256(helper.read_bytes()).hexdigest()
            with patch.object(b,'READER_SHA',pin):
                for name in ['a.tar.gz','b.tar.gz']:b.build(helper,root/name)
            self.assertEqual((root/'a.tar.gz').read_bytes(),(root/'b.tar.gz').read_bytes())
            with tarfile.open(root/'a.tar.gz') as t:
                self.assertEqual(t.getnames(),['manifest.json','publish_backup_health.py','bin/health-reader'])
                self.assertTrue(all(x.isfile() for x in t.getmembers()))
                self.assertEqual(t.extractfile('bin/health-reader').read(),helper.read_bytes())
                m=json.load(t.extractfile('manifest.json'));self.assertFalse(m['installation_ready'])
                self.assertEqual(m['files']['bin/health-reader']['sha256'],pin)
                self.assertIn('qualified service/timer/environment and rollout custody',m['prerequisites'])
    def test_wrong_missing_symlink_inputs_leave_no_output(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=pathlib.Path(tmp);helper=self.fixture(root);alias=root/'alias';alias.symlink_to(helper)
            for path in [helper,root/'missing',alias]:
                with self.subTest(path=path),self.assertRaises((ValueError,FileNotFoundError)):
                    b.build(path,root/'out')
                self.assertFalse((root/'out').exists())
            self.assertEqual(sorted(x.name for x in root.iterdir()),['alias','reader'])
    def test_existing_output_not_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=pathlib.Path(tmp);helper=self.fixture(root);out=root/'out';out.write_bytes(b'keep')
            with patch.object(b,'READER_SHA',hashlib.sha256(helper.read_bytes()).hexdigest()),self.assertRaises(FileExistsError):b.build(helper,out)
            self.assertEqual(out.read_bytes(),b'keep');self.assertEqual(sorted(x.name for x in root.iterdir()),['out','reader'])
    def test_changed_publisher_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=pathlib.Path(tmp);helper=self.fixture(root);src=root/'script';src.write_bytes(b'changed')
            with self.assertRaises(ValueError):b.build(helper,root/'out',src)
            self.assertFalse((root/'out').exists())

    @unittest.skipUnless(os.environ.get('FC_HEALTH_HELPER'),'explicit qualified helper input')
    def test_actual_qualified_reader_bundle(self):
        with tempfile.TemporaryDirectory() as tmp:
            out=pathlib.Path(tmp)/'actual.tar.gz';b.build(pathlib.Path(os.environ['FC_HEALTH_HELPER']),out)
            with tarfile.open(out) as t:
                self.assertEqual(hashlib.sha256(t.extractfile('bin/health-reader').read()).hexdigest(),b.READER_SHA)
                self.assertEqual(hashlib.sha256(t.extractfile('publish_backup_health.py').read()).hexdigest(),b.PUBLISHER_SHA)

if __name__=='__main__':unittest.main()