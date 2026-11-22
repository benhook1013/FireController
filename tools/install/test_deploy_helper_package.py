import hashlib
import io
import json
from pathlib import Path
import tarfile
import unittest
import subprocess
import tempfile
from unittest.mock import patch
import deploy_helper_package as p


class DeploymentPackageTests(unittest.TestCase):
    def sources(self):
        return {name: b'import os\n' for name in p.MODULES}

    def test_missing_extra_and_external_refuse(self):
        for change in ('missing', 'extra', 'external', 'relative', 'syntax'):
            with self.subTest(change=change):
                data = self.sources()
                if change == 'missing': data.pop('kubernetes')
                if change == 'extra': data['test_helper'] = b''
                if change == 'external': data['kubernetes'] = b'import undeclared_package\n'
                if change == 'relative': data['kubernetes'] = b'from . import helper\n'
                if change == 'syntax': data['kubernetes'] = b'def invalid('
                with self.assertRaises(p.Invalid): p.inspect_sources(data)

    def test_deterministic_complete_archive_and_false_authority(self):
        data = self.sources()
        data['kubernetes'] = b'import artifact_operation\n'
        with patch.object(p, 'source_at', return_value=data):
            raw, manifest = p.assemble('/unused', 'a' * 40)
            self.assertEqual(raw, p.assemble('/unused', 'a' * 40)[0])
        self.assertEqual(manifest['local_imports']['kubernetes'], ['artifact_operation'])
        self.assertFalse(manifest['installation_ready'])
        self.assertFalse(manifest['closure_verified'])
        with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
            self.assertEqual(len(archive.getmembers()), 22)
            for member in archive:
                self.assertTrue(member.isfile())
                body = archive.extractfile(member).read()
                if member.name != 'manifest.json':
                    self.assertEqual(hashlib.sha256(body).hexdigest(), manifest['sha256'][member.name])
                else: self.assertEqual(json.loads(body), manifest)

    def test_retained_sources_are_packaged_from_git(self):
        repo = Path(__file__).resolve().parents[2]
        for revision, count in (
            ('81b2906e45c6f9cc9ff51bc35cee1ff31b88eed0', 17),
            ('0fbc6d9afab241761145cd1e272f14d529f2d891', 20),
            ('31c9ff51c77f82ea05231a35958f1cf616f0decb', 20),
        ):
            with self.subTest(revision=revision):
                raw, manifest = p.assemble(repo, revision)
                self.assertEqual(len(manifest['sha256']), count)
                self.assertGreater(len(raw), 100000)
                self.assertEqual(manifest['source'], revision)

    def test_nonimmutable_revision_refused(self):
        for revision in ('HEAD', '81b2906e', '-' + 'a' * 39):
            with self.assertRaises(p.Invalid): p.source_at('/unused', revision)


class ExactGitObjectTests(unittest.TestCase):
    def test_commit_and_blob_replace_refs_cannot_substitute_package_bytes(self):
        for replacement in ('commit', 'blob'):
            with self.subTest(replacement=replacement), tempfile.TemporaryDirectory() as directory:
                repo = Path(directory)
                def git(*args):
                    return subprocess.check_output(['git', '-C', str(repo), *args], stderr=subprocess.DEVNULL).decode().strip()
                git('init')
                git('config', 'user.name', 'Fixture')
                git('config', 'user.email', 'fixture@example.invalid')
                target = repo / 'tools/deploy'
                target.mkdir(parents=True)
                for name in p.MODULES:
                    (target / (name + '.py')).write_bytes(b'import os\n')
                git('add', '.')
                git('commit', '-m', 'original')
                original = git('rev-parse', 'HEAD')
                original_blob = git('rev-parse', 'HEAD:tools/deploy/kubernetes.py')
                (target / 'kubernetes.py').write_bytes(b'import sys\n')
                git('add', '.')
                git('commit', '-m', 'substitute')
                substitute = git('rev-parse', 'HEAD')
                substitute_blob = git('rev-parse', 'HEAD:tools/deploy/kubernetes.py')
                if replacement == 'commit': git('replace', original, substitute)
                else: git('replace', original_blob, substitute_blob)
                self.assertEqual(git('show', original + ':tools/deploy/kubernetes.py'), 'import sys')
                self.assertEqual(p.source_at(repo, original)['kubernetes'], b'import os\n')
