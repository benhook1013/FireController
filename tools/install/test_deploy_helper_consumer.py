import hashlib
import io
import json
from pathlib import Path
import tarfile
import unittest
from unittest.mock import patch
import deploy_helper_package as p
import deploy_helper_consumer as c


class ConsumerTests(unittest.TestCase):
    def archive(self):
        with patch.object(p, 'source_at', return_value={name:b'import os\n' for name in p.MODULES}):
            return p.assemble('/unused', 'a'*40)[0]

    def mutate(self, raw, kind):
        output = io.BytesIO()
        with tarfile.open(fileobj=io.BytesIO(raw)) as src, tarfile.open(fileobj=output, mode='w') as dst:
            for member in src:
                body = src.extractfile(member).read()
                if member.name == 'manifest.json':
                    manifest = json.loads(body)
                    if kind == 'authority': manifest['installation_ready'] = True
                    if kind == 'source': manifest['source'] = 'b'*40
                    body = json.dumps(manifest).encode()
                    member.size = len(body)
                elif member.name == 'kubernetes.py':
                    if kind == 'missing': continue
                    if kind == 'path': member.name = '../kubernetes.py'
                    if kind == 'contents': body = b'x'*len(body)
                    if kind == 'link': member.type = tarfile.SYMTYPE; member.linkname = '/etc/passwd'
                dst.addfile(member, io.BytesIO(body))
                if kind == 'duplicate' and member.name == 'kubernetes.py': dst.addfile(member, io.BytesIO(body))
        return output.getvalue()

    def test_real_current_package_consumed_without_switch_authority(self):
        revision = '81b2906e45c6f9cc9ff51bc35cee1ff31b88eed0'
        raw, _ = p.assemble(Path(__file__).resolve().parents[2], revision)
        result = c.verify(raw, hashlib.sha256(raw).hexdigest(), revision)
        self.assertEqual(result['verified_modules'], 17)
        self.assertFalse(result['platform_verified'])
        self.assertFalse(result['generation_switch_ready'])

    def test_tampering_refused_even_when_outer_digest_reselected(self):
        raw = self.archive()
        for kind in ('authority','source','missing','path','contents','link','duplicate'):
            with self.subTest(kind=kind):
                changed = self.mutate(raw, kind)
                with self.assertRaises(c.Invalid): c.verify(changed, hashlib.sha256(changed).hexdigest(), 'a'*40)

    def test_wrong_external_digest_refused(self):
        with self.assertRaises(c.Invalid): c.verify(self.archive(), '0'*64, 'a'*40)
