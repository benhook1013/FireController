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

import artifact_schedule as schedule
from artifact_retention import Refuse


class ScheduleTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.root = self.base / "artifacts"
        self.results = self.base / "results"
        self.root.mkdir(mode=0o700)
        self.results.mkdir(mode=0o700)
        self.lock = self.base / "lease"
        self.lock.touch(mode=0o600)
        self.config = self.base / "config.json"
        self.data = {"root": str(self.root), "manifest": str(self.root / "manifest.json"), "current_revision": "a"*40,
                     "verified_rollback_revision": "b"*40, "valid_until": time.time()+600}
        self.write()

    def write(self):
        self.config.write_text(json.dumps(self.data))
        self.config.chmod(0o600)

    def detail(self):
        return {"mode":"dry-run", "files":[{"state":"eligible"}], "verified_bytes":8,
                "reclaimed_allocated_bytes":0, "free_bytes":10, "headroom_alert":True}

    def execute(self):
        return schedule.execute(self.config, self.lock, self.results)

    def test_fifo_state_refuses_without_waiting_for_writer(self):
        # Run in a child so a regression cannot hang the test suite itself.
        for target in (self.config, self.results / "latest.json", self.results / "previous.json"):
            with self.subTest(target=target.name):
                if target.exists():
                    target.unlink()
                os.mkfifo(target, 0o600)
                try:
                    child = subprocess.run([sys.executable, "-c", """
import sys
from pathlib import Path
import artifact_schedule as s
from artifact_retention import Refuse
try:
    if sys.argv[2] == 'config.json':
        s.configuration(Path(sys.argv[1]), __import__('time').time())
    else:
        s.validate_result_store(Path(sys.argv[1]).parent)
except Refuse:
    sys.exit(0)
raise AssertionError('FIFO was not refused')
""", str(target), target.name], cwd=Path(schedule.__file__).parent,
                        capture_output=True, text=True, timeout=3)
                    self.assertEqual(child.returncode, 0, child.stderr)
                    self.assertTrue(target.exists())
                finally:
                    target.unlink()
                    if target == self.config:
                        self.write()

    def test_default_dryrun_and_same_lock_custody(self):
        def collect(a, held):
            self.assertFalse(a.apply)
            self.assertEqual(a.current_revision,"a"*40)
            with self.lock.open() as competing:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(competing,fcntl.LOCK_EX|fcntl.LOCK_NB)
            return self.detail()
        with patch.object(schedule,"collect_locked",side_effect=collect):
            result=self.execute()
        self.assertEqual(result["files_eligible"],1)
        self.assertTrue(result["headroom_alert"])
        self.assertNotIn("files",result)
        self.assertLess((self.results/"latest.json").stat().st_size,4096)

    def test_explicit_apply_only(self):
        self.data["apply"]=True;self.write()
        with patch.object(schedule,"collect_locked",return_value=self.detail()) as collect:
            self.execute()
        self.assertTrue(collect.call_args.args[0].apply)
        self.data["apply"]="true";self.write()
        with patch.object(schedule,"collect_locked") as collect,self.assertRaises(Refuse):
            self.execute()
        collect.assert_not_called()

    def test_expiry_unknown_fields_and_budget_fail_closed(self):
        for change in ({"valid_until":time.time()-1},{"valid_until":float('nan')},{"valid_until":time.time()+90000},{"max_verify_bytes":2**30+1},{"max_verify_bytes":True},{"purge":True}):
            original=self.data.copy();self.data.update(change);self.write()
            with self.subTest(change=change),patch.object(schedule,"collect_locked") as collect,self.assertRaises(Refuse):
                self.execute()
            collect.assert_not_called();self.data=original

    def test_result_retention_exactly_two_files(self):
        with patch.object(schedule,"collect_locked",return_value=self.detail()):
            for _ in range(4):self.execute()
        self.assertEqual(sorted(p.name for p in self.results.iterdir()),["latest.json","previous.json"])
        self.assertEqual((self.results/"latest.json").stat().st_mode & 0o777,0o600)

    def test_collector_failure_saves_no_sensitive_error(self):
        with patch.object(schedule,"collect_locked",side_effect=Refuse("SECRET/private/path")),self.assertRaises(Refuse):
            self.execute()
        body=(self.results/"latest.json").read_text()
        self.assertNotIn("SECRET",body)
        self.assertEqual(json.loads(body)["state"],"refused")

    def test_results_in_enrollment_never_written(self):
        self.data["root"]=str(self.base);self.write()
        with patch.object(schedule,"collect_locked") as collect,self.assertRaises(Refuse):self.execute()
        collect.assert_not_called()
        self.assertEqual(list(self.results.iterdir()),[])

    def test_busy_lease_no_collection_or_results(self):
        with self.lock.open() as held:
            fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
            with patch.object(schedule,"collect_locked") as collect,self.assertRaises(BlockingIOError):self.execute()
        collect.assert_not_called()
        self.assertEqual(list(self.results.iterdir()),[])

    def test_symlink_or_unfinished_result_fails_closed(self):
        target=self.base/"untouched";target.write_text("private")
        (self.results/"latest.json").symlink_to(target)
        with self.assertRaises(OSError):schedule.save_result(self.results,{"state":"complete"})
        self.assertEqual(target.read_text(),"private")
        (self.results/"latest.json").unlink()
        (self.results/"pending.json").write_text("unfinished")
        with self.assertRaises(FileExistsError):schedule.save_result(self.results,{"state":"complete"})
        self.assertEqual((self.results/"pending.json").read_text(),"unfinished")

    def test_entrypoint_import_and_service_safety(self):
        p=subprocess.run([sys.executable,str(Path(schedule.__file__)),"--help"],capture_output=True)
        self.assertEqual(p.returncode,0,p.stderr)
        unit=Path(schedule.__file__).with_name("firecontroller-artifact-retention.service").read_text()
        self.assertIn("KillMode=control-group",unit)
        self.assertIn("TimeoutStartSec=120",unit)
        self.assertNotIn("--apply",unit)
        self.assertNotIn("PrivateTmp",unit)
        self.assertNotIn("ProtectProc",unit)

    def test_unfinished_result_blocks_before_collection(self):
        (self.results/"pending.json").write_text("unfinished")
        with patch.object(schedule,"collect_locked") as collect,self.assertRaises(Refuse):self.execute()
        collect.assert_not_called()

    def test_real_copy_consumer_lease_then_exact_redundant_copy_apply(self):
        import hashlib
        import artifact_operation
        recovery = self.root / "recovery"
        payload = b"reproducible local rehearsal input"
        recovery.write_bytes(payload)
        manifest = self.root / "manifest.json"
        entries = [
            {"path": name, "recovery_path": "recovery",
             "sha256": hashlib.sha256(payload).hexdigest(), "revision": revision,
             "kind": "rehearsal-copy", "terminal": True, "expires_at": time.time()-1}
            for name, revision in (("copy", "c"*40), ("current", "a"*40), ("rollback", "b"*40))
        ]
        manifest.write_text(json.dumps({"version": 1, "workflow_receipt": "terminal.json",
                                        "artifacts": entries}))
        artifact_operation.execute(self.root, self.lock, [sys.executable, "-c",
            "import shutil; "
            "[shutil.copyfile('recovery', name) for name in ('copy','current','rollback')]"], manifest)
        for name in ("copy", "current", "rollback"):
            self.assertEqual((self.root/name).read_bytes(), payload)
        receipt = (self.root/"terminal.json").read_bytes()
        ready = self.base / "consumer-ready"
        # Close the input after reading it: the shared lease must still protect it
        # for the rest of the foreground consumer's lifetime, not only open FDs.
        consumer = subprocess.Popen([
            sys.executable, str(Path(artifact_operation.__file__)),
            "--root", str(self.root), "--lock-file", str(self.lock), "--consumer", "--",
            sys.executable, "-c",
            "import select,sys; from pathlib import Path; "
            "assert Path('copy').read_bytes() == " + repr(payload) + "; "
            "Path(sys.argv[1]).write_text('read'); "
            "assert select.select([sys.stdin], [], [], 10)[0]; assert sys.stdin.read(1) == 'x'",
            str(ready)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            deadline = time.monotonic()+5
            while not ready.exists() and consumer.poll() is None and time.monotonic() < deadline:
                time.sleep(.01)
            self.assertTrue(ready.exists(), "consumer did not reach the actual read barrier")
            self.assertIsNone(consumer.poll())
            self.data["apply"] = True
            self.write()
            # No process-inventory mock on this path: refusal must precede collection.
            with patch.object(schedule, "collect_locked") as collect:
                with self.assertRaises(BlockingIOError):
                    self.execute()
            collect.assert_not_called()
            self.assertEqual(list(self.results.iterdir()), [])
            self.assertEqual((self.root/"copy").read_bytes(), payload)
            self.assertEqual((self.root/"terminal.json").read_bytes(), receipt)
        finally:
            output, error = consumer.communicate(b"x", timeout=15)
        self.assertEqual(consumer.returncode, 0, error)
        self.assertEqual(json.loads(output)["state"], "consumer_finished")
        # Process-inventory behavior has separate tests. This fixture controls only
        # local lease custody; it does not claim a production host inventory.
        with patch("artifact_retention.active_paths", return_value=[]):
            self.data["apply"] = False
            self.write()
            dry = self.execute()
            self.assertEqual(dry["mode"], "dry-run")
            self.assertTrue((self.root/"copy").exists())
            self.data["apply"] = True
            self.write()
            applied = self.execute()
        self.assertEqual(applied["files_removed"], 1)
        self.assertFalse((self.root/"copy").exists())
        for name in ("recovery", "current", "rollback"):
            self.assertEqual((self.root/name).read_bytes(), payload)
        self.assertEqual((self.root/"terminal.json").read_bytes(), receipt)

    def test_successful_sqlite_consumer_cannot_enroll_changed_rehearsal_output(self):
        import hashlib
        import sqlite3
        import artifact_operation
        recovery = self.root / "recovery.sqlite"
        with sqlite3.connect(recovery) as db:
            db.execute("CREATE TABLE retained (value TEXT NOT NULL)")
            db.execute("INSERT INTO retained VALUES ('original')")
        before = recovery.read_bytes()
        manifest = self.root / "manifest.json"
        manifest.write_text(json.dumps({"version": 1, "workflow_receipt": "terminal.json", "artifacts": [
            {"path": "copy.sqlite", "recovery_path": "recovery.sqlite",
             "sha256": hashlib.sha256(before).hexdigest(), "revision": "c"*40,
             "kind": "rehearsal-copy", "terminal": True, "expires_at": time.time()-1}]}))
        artifact_operation.execute(self.root, self.lock, [sys.executable, "-c",
            "import shutil; shutil.copyfile('recovery.sqlite', 'copy.sqlite')"], manifest)
        receipt = (self.root/"terminal.json").read_bytes()
        result = artifact_operation.execute(self.root, self.lock, [sys.executable, "-c",
            "import sqlite3; db=sqlite3.connect('copy.sqlite'); "
            "db.execute('CREATE TABLE rehearsal_schema (value TEXT)'); db.commit(); db.close()"],
            consumer=True)
        self.assertEqual(result["state"], "consumer_finished")
        changed = (self.root/"copy.sqlite").read_bytes()
        self.assertNotEqual(changed, before)
        with sqlite3.connect(self.root/"copy.sqlite") as db:
            self.assertEqual(db.execute("SELECT value FROM retained").fetchall(), [("original",)])
            self.assertEqual(db.execute("SELECT COUNT(*) FROM rehearsal_schema").fetchone(), (0,))
        self.data["apply"] = True
        self.write()
        with patch("artifact_retention.active_paths", return_value=[]):
            with self.assertRaisesRegex(Refuse, "equivalent-copy hash verification failed"):
                self.execute()
        self.assertEqual((self.root/"copy.sqlite").read_bytes(), changed)
        self.assertEqual(recovery.read_bytes(), before)
        self.assertEqual((self.root/"terminal.json").read_bytes(), receipt)
        self.assertEqual(json.loads((self.results/"latest.json").read_text())["state"], "refused")
