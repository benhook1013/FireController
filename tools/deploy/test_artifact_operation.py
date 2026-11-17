import fcntl
import hashlib
import json
import os
from pathlib import Path
import sys
import signal
import subprocess
import time
import tempfile
import unittest

import artifact_operation as operation
from artifact_retention import Refuse


class OperationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.lock = self.root / "lease"
        self.lock.touch()
        self.manifest = self.root / "manifest.json"
        self.manifest.write_text(json.dumps({"version": 1, "workflow_receipt": "terminal.json", "artifacts": []}))

    def execute(self, code="pass", **kwargs):
        return operation.execute(self.root, self.lock, [sys.executable, "-c", code],
                                 kwargs.pop("manifest", self.manifest), **kwargs)

    def test_terminal_receipt_binds_manifest_and_lock(self):
        result = self.execute()
        self.assertEqual(result["state"], "terminal")
        self.assertEqual(result["manifest_sha256"], hashlib.sha256(self.manifest.read_bytes()).hexdigest())
        self.assertEqual(result["lock_identity"], [self.lock.stat().st_dev, self.lock.stat().st_ino])
        with self.assertRaises(Refuse):
            self.execute()

    def test_failure_never_issues_receipt(self):
        with self.assertRaises(Refuse):
            self.execute("raise SystemExit(1)")
        self.assertFalse((self.root / "terminal.json").exists())

    def test_producer_cannot_run_during_consumer_lease(self):
        with self.lock.open() as lock:
            fcntl.flock(lock, fcntl.LOCK_SH | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                self.execute()

    def test_consumer_does_not_issue_receipt(self):
        self.assertEqual(self.execute(manifest=None, consumer=True)["state"], "consumer_finished")
        self.assertFalse((self.root / "terminal.json").exists())

    def test_manifest_mutation_refused(self):
        with self.assertRaises(Refuse):
            self.execute("from pathlib import Path; Path('manifest.json').write_text('{}')")
        self.assertFalse((self.root / "terminal.json").exists())

    def test_symlink_receipt_refused(self):
        (self.root / "terminal.json").symlink_to(self.root / "absent")
        with self.assertRaises(Refuse):
            self.execute()

    def test_child_keeps_lease_when_wrapper_is_killed(self):
        command = [sys.executable, str(Path(operation.__file__)), "--root", str(self.root),
                   "--lock-file", str(self.lock), "--manifest", str(self.manifest), "--",
                   sys.executable, "-c",
                   "import os,time; from pathlib import Path; Path('pid').write_text(str(os.getpid())); time.sleep(20)"]
        wrapper = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        child = None
        try:
            for _ in range(100):
                if (self.root / "pid").exists():
                    child = int((self.root / "pid").read_text())
                    break
                time.sleep(.02)
            self.assertIsNotNone(child)
            wrapper.kill()
            wrapper.wait(timeout=3)
            with self.lock.open() as lock:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.assertFalse((self.root / "terminal.json").exists())
        finally:
            if wrapper.poll() is None:
                wrapper.kill()
                wrapper.wait(timeout=3)
            if child is not None:
                try:
                    os.kill(child, signal.SIGTERM)
                except ProcessLookupError:
                    pass


if __name__ == "__main__":
    unittest.main()
