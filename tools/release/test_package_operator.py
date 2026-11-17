import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('operator_package',Path(__file__).with_name('package-operator.py'))
p=importlib.util.module_from_spec(spec);spec.loader.exec_module(p)
REV='a'*40
class OperatorPackageTests(unittest.TestCase):
    def test_missing_wrong_reader_and_existing_output_precede_native_packaging(self):
        with tempfile.TemporaryDirectory() as tmp,patch.object(p.clients,'package') as native:
            root=Path(tmp);reader=root/'reader';output=root/'output'
            for content in (None,b'wrong reader'):
                if content is not None:reader.write_bytes(content)
                with self.assertRaises((OSError,ValueError)):p.package(root,reader,output,REV)
                self.assertFalse(output.exists());native.assert_not_called()
            output.write_bytes(b'keep')
            with self.assertRaises(FileExistsError):p.package(root,reader,output,REV)
            self.assertEqual(output.read_bytes(),b'keep');native.assert_not_called()

    @unittest.skipUnless(os.environ.get('FC_HEALTH_HELPER'),'qualified reader input unavailable')
    def test_companion_pin_manifest_and_native_failure(self):
        # Native packaging is a stub here; its independent provenance checks are
        # covered by its own tests, not claimed exercised by this adapter test.
        reader=Path(os.environ['FC_HEALTH_HELPER'])
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);output=root/'operator.tar.gz'
            with patch.object(p.clients,'package',side_effect=ValueError('native qualification failed')):
                with self.assertRaisesRegex(ValueError,'native qualification'):p.package(root,reader,output,REV)
            self.assertEqual(list(root.iterdir()),[])
            def native(binary,platform,destination,revision):
                self.assertEqual((platform,revision),('linux-amd64',REV))
                (destination/'firecontroller-linux-amd64.tar.gz').write_bytes(b'native fixture')
            with patch.object(p.clients,'package',side_effect=native):
                manifest=p.package(root,reader,output,REV)
            self.assertFalse(manifest['installation_ready'])
            self.assertEqual(manifest['health_reader_sha256'],p.health.READER_SHA)
            with tarfile.open(output) as archive:
                self.assertEqual(archive.getnames(),['firecontroller-linux-amd64.tar.gz','operator-health-linux-amd64.tar.gz','operator-release.json'])
                self.assertEqual(json.load(archive.extractfile('operator-release.json')),manifest)
                for name,record in manifest['artifacts'].items():
                    body=archive.extractfile(name).read()
                    self.assertEqual(hashlib.sha256(body).hexdigest(),record['sha256'])
                    self.assertEqual(len(body),record['bytes'])
            self.assertEqual([x.name for x in root.iterdir()],['operator.tar.gz'])

if __name__=='__main__':unittest.main()