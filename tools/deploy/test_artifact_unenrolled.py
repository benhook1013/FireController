import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import artifact_operation as operation
from artifact_retention import Refuse, open_lease


class UnenrolledProducerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.lock = self.root / 'artifact.lease'
        self.lock.touch(mode=0o600)

    def test_zero_copy_producer_finishes_without_enrollment_or_receipt(self):
        result = operation.execute(self.root, self.lock,
            [sys.executable, '-c', "from pathlib import Path; Path('image.tar').write_bytes(b'new image archive')"],
            producer_only=True)
        self.assertEqual(result, {'state': 'producer_finished_unenrolled'})
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ['artifact.lease', 'image.tar'])
        self.assertEqual((self.root/'image.tar').read_bytes(), b'new image archive')

    def test_unenrolled_mode_rejects_manifest_and_consumer_combination(self):
        for opts in ({'manifest_path': self.root/'invented.json'}, {'consumer': True}):
            with self.assertRaises(Refuse):
                operation.execute(self.root, self.lock, [sys.executable, '-c', 'pass'], producer_only=True, **opts)
        self.assertEqual(list(self.root.iterdir()), [self.lock])

    def test_required_handoff_rejects_missing_invalid_and_closed_descriptors(self):
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(operation.lease_subprocess_options(), {})
            with self.assertRaises(Refuse):
                operation.lease_subprocess_options(required=True)
        for raw in ('1', '-1', 'garbage', '9999999999'):
            with self.subTest(raw=raw), patch.dict(os.environ, {'FC_ARTIFACT_LEASE_FD': raw}):
                with self.assertRaises(Refuse):
                    operation.lease_subprocess_options(required=True)

    def test_failed_producer_preserves_original_archive_without_receipt(self):
        with self.assertRaises(Refuse):
            operation.execute(self.root, self.lock,
                [sys.executable, '-c', "from pathlib import Path; Path('image.tar').write_bytes(b'partial'); raise SystemExit(3)"],
                producer_only=True)
        self.assertEqual((self.root/'image.tar').read_bytes(), b'partial')
        self.assertEqual(len(list(self.root.iterdir())), 2)

    def test_nested_import_keeps_exact_lease_after_wrapper_death(self):
        ready, finish = self.root/'ready', self.root/'finish'
        leaf = ("import os,time,json; from pathlib import Path; "
            "fd=int(os.environ['FC_ARTIFACT_LEASE_FD']); s=os.fstat(fd); "
            "Path('ready').write_text(json.dumps([s.st_dev,s.st_ino])); "
            "exec('for _ in range(1000):\\n if Path(\"finish\").exists(): break\\n time.sleep(.01)\\nelse: raise RuntimeError(\"import timed out\")')")
        middle = ("import sys,subprocess; sys.path.insert(0,"+repr(str(Path(operation.__file__).parent))+"); "
            "from artifact_operation import lease_subprocess_options; "
            "subprocess.run([sys.executable,'-c',"+repr(leaf)+"],check=True,**lease_subprocess_options(required=True))")
        wrapper = subprocess.Popen([sys.executable, str(Path(operation.__file__)), '--root', str(self.root),
            '--lock-file', str(self.lock), '--producer-only', '--', sys.executable, '-c', middle],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(200):
                if ready.exists() or wrapper.poll() is not None:
                    break
                time.sleep(.01)
            self.assertTrue(ready.exists())
            self.assertEqual(json.loads(ready.read_text()), [self.lock.stat().st_dev,self.lock.stat().st_ino])
            wrapper.kill()
            wrapper.wait(timeout=3)
            with open_lease(self.lock) as lease:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(lease,fcntl.LOCK_EX|fcntl.LOCK_NB)
            finish.touch()
            released = False
            for _ in range(200):
                with open_lease(self.lock) as lease:
                    try:
                        fcntl.flock(lease,fcntl.LOCK_EX|fcntl.LOCK_NB)
                        released = True
                        break
                    except BlockingIOError:
                        time.sleep(.01)
            self.assertTrue(released, 'nested child did not release custody after finishing')
            self.assertFalse(any(p.name.endswith('.json') for p in self.root.iterdir()))
        finally:
            finish.touch(exist_ok=True)
            if wrapper.poll() is None:
                wrapper.wait(timeout=5)


if __name__ == '__main__':
    unittest.main()
