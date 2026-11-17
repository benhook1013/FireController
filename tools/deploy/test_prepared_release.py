import fcntl
import os
from pathlib import Path
import tempfile
import unittest

import prepared_release as p


class PreparedReleaseTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.lease = self.root / 'release.lock'
        self.lease.touch(mode=0o600)
        self.identity = {'source': 'exact-reviewed-source', 'inputs': 'exact-digest'}
        self.current = {'uid': 'deployment', 'generation': 1}
        self.events = []

    def verify(self, fd):
        self.assertEqual(os.fstat(fd).st_ino, self.lease.stat().st_ino)
        return dict(self.current)

    def build(self, fd):
        self.events.append('build')
        (self.root / 'image-manifest.json').write_text('{"image":"qualified"}')

    def prepare(self):
        return p.prepare(self.root, self.lease, self.identity, self.verify, self.build)

    def admit(self, window=lambda: None):
        return p.admit(self.root, self.lease, self.identity, self.verify, window,
                       lambda fd: self.events.append('rollout'))

    def test_prepare_then_defer_and_prompt_admission_builds_once(self):
        self.prepare()
        def closed():
            raise ValueError('existing 20m window closed')
        with self.assertRaises(ValueError):
            self.admit(closed)
        self.assertFalse((self.root/'admission-started.json').exists())
        self.admit()
        self.assertEqual(self.events, ['build', 'rollout'])
        with self.assertRaises(FileExistsError):
            self.admit()
        self.assertEqual(self.events, ['build', 'rollout'])

    def test_unknown_preparation_never_replayed(self):
        def failed(fd):
            self.build(fd)
            raise RuntimeError('producer outcome unknown')
        with self.assertRaises(RuntimeError):
            p.prepare(self.root,self.lease,self.identity,self.verify,failed)
        self.assertFalse((self.root/'prepared-image.json').exists())
        with self.assertRaises(FileExistsError):
            self.prepare()
        with self.assertRaises(FileNotFoundError):
            self.admit()
        self.assertEqual(self.events,['build'])

    def test_changed_preimage_output_or_identity_refuses(self):
        for change in ['preimage','output','identity','during_window']:
            with self.subTest(change=change), tempfile.TemporaryDirectory() as tmp:
                oldroot=self.root;self.root=Path(tmp)
                try:
                    self.prepare()
                    if change=='preimage': self.current['generation']+=1
                    if change=='output': (self.root/'image-manifest.json').write_text('different')
                    if change=='identity': self.identity['source']='changed'
                    def window():
                        if change=='during_window':self.current['generation']+=1
                    with self.assertRaises(ValueError):self.admit(window)
                    self.assertFalse((self.root/'admission-started.json').exists())
                finally:self.root=oldroot

    def test_active_release_refuses_before_production(self):
        with self.lease.open('r+') as held:
            fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):self.prepare()
        self.assertEqual(self.events,[])
        self.assertFalse((self.root/'preparation-started.json').exists())

    def test_preparation_does_not_acquire_backup_lease(self):
        backup=self.root/'backup.lock';backup.touch()
        with backup.open('r+') as held:
            fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.prepare()
        self.assertEqual(self.events,['build'])

    def test_window_replaced_lease_refuses_before_rollout(self):
        self.prepare()
        def window():
            self.lease.rename(self.root/'old-release.lock')
            self.lease.touch(mode=0o600)
        # verify's documented contract returns preimage; the helper itself must
        # enforce lease identity rather than relying on this callback to do it.
        with self.assertRaises(ValueError):
            p.admit(self.root,self.lease,self.identity,lambda fd:dict(self.current),window,
                    lambda fd:self.events.append('rollout'))
        self.assertEqual(self.events,['build'])
        self.assertFalse((self.root/'admission-started.json').exists())

    def test_changed_lease_before_build_or_receipt_fences(self):
        def replace():
            self.lease.rename(self.root/'old-release.lock')
            self.lease.touch(mode=0o600)
        def verify(fd):
            replace()
            return dict(self.current)
        with self.assertRaises(ValueError):
            p.prepare(self.root,self.lease,self.identity,verify,self.build)
        self.assertEqual(self.events,[])
        self.assertFalse((self.root/'preparation-started.json').exists())


if __name__=='__main__':unittest.main()
