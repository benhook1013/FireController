import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
import rendered_operation as subject

class TestRenderedOperation(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.source = 'a' * 40
        self.image = 'localhost/firecontroller@sha256:' + 'b' * 64
        (self.root / 'release/bin').mkdir(parents=True)
        (self.root / 'release/bin/fcd').write_bytes(b'synthetic binary, never executed')
        self.binary = self.sha(self.root / 'release/bin/fcd')
        self.proof = {'source': self.source, 'binary_sha256': self.binary}
        self.receipt = dict(self.proof, image=self.image)
        self.write('canonical-proof.json', self.proof)
        self.write('image-manifest.json', self.receipt)
        self.pin()
    def sha(self, path):
        return hashlib.sha256(path.read_bytes()).hexdigest()
    def write(self, name, value):
        (self.root / name).write_text(json.dumps(value))
    def pin(self, extras=None):
        self.hashes = {p: self.sha(self.root / p) for p in ('canonical-proof.json', 'release/bin/fcd')}
        self.hashes.update(extras or {})
        self.write('execution-hashes.json', self.hashes)
        self.digest = self.sha(self.root / 'execution-hashes.json')
    def run_inspect(self):
        return subject.inspect(str(self.root), self.digest, self.source, self.image)
    def test_valid_not_install_authority(self):
        result = self.run_inspect()
        self.assertTrue(result['receipt_bytes_consistent'])
        self.assertFalse(result['installation_ready'])
        self.assertFalse(result['dependency_closure_verified'])
    def test_wrong_later_receipts(self):
        self.run_inspect()
        for key in ('source', 'binary_sha256', 'image'):
            with self.subTest(key=key):
                self.write('image-manifest.json', dict(self.receipt, **{key:'wrong'}))
                with self.assertRaises(ValueError): self.run_inspect()
        self.write('image-manifest.json', self.receipt)
        self.run_inspect()
    def test_tamper_and_manifest_pin(self):
        (self.root / 'release/bin/fcd').write_bytes(b'changed')
        with self.assertRaises(ValueError): self.run_inspect()
        self.write('execution-hashes.json', {})
        with self.assertRaises(ValueError): self.run_inspect()
    def test_path_escape_and_symlink(self):
        for name in ('../escape', '/absolute', 'release//bin/fcd'):
            self.pin({name:'c'*64})
            with self.assertRaises(ValueError): self.run_inspect()
        self.pin()
        (self.root/'release/bin/fcd').unlink()
        (self.root/'release/bin/fcd').symlink_to('/dev/null')
        with self.assertRaises(OSError): self.run_inspect()
    def test_duplicate_keys(self):
        (self.root/'image-manifest.json').write_text('{"source":"a","source":"b"}')
        with self.assertRaises(ValueError): self.run_inspect()
    def test_replaced_intermediate_directory(self):
        original = subject.member
        calls = 0
        def changed(fd, name, limit):
            nonlocal calls
            result = original(fd, name, limit)
            if name == 'release/bin/fcd':
                calls += 1
                if calls == 1:
                    os.rename(self.root/'release/bin', self.root/'release/old')
                    (self.root/'release/bin').mkdir()
                    (self.root/'release/bin/fcd').write_bytes((self.root/'release/old/fcd').read_bytes())
            return result
        from unittest.mock import patch
        with patch.object(subject, 'member', changed):
            with self.assertRaises(ValueError): self.run_inspect()

if __name__ == '__main__': unittest.main()
