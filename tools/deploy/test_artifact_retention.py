import hashlib
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("retention", Path(__file__).with_name("artifact_retention.py"))
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)


class RetentionTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / "old").write_bytes(b"reproducible")
        (self.root / "safe").write_bytes(b"reproducible")
        self.entry = dict(path="old", recovery_path="safe", revision="3" * 40,
                          sha256=hashlib.sha256(b"reproducible").hexdigest(),
                          kind="build", terminal=True, expires_at=10)

    def run_gc(self, **kwargs):
        return r.run(self.root, {"version": 1, "artifacts": [self.entry]},
                     "1" * 40, "2" * 40, kwargs.pop("references", []), now=20, **kwargs)

    def test_dry_run_preserves_both_copies(self):
        self.assertEqual(self.run_gc()["files"][0]["state"], "eligible")
        self.assertTrue((self.root / "old").exists())
        self.assertTrue((self.root / "safe").exists())

    def test_apply_removes_only_verified_duplicate_and_replays(self):
        self.assertEqual(self.run_gc(apply=True)["files"][0]["state"], "removed")
        self.assertEqual((self.root / "safe").read_bytes(), b"reproducible")
        self.assertEqual(self.run_gc(apply=True)["files"][0]["reason"], "already_absent")

    def test_current_and_rollback_preserved(self):
        for revision in ("1" * 40, "2" * 40):
            self.entry["revision"] = revision
            self.assertEqual(self.run_gc(apply=True)["files"][0]["reason"], "current_or_rollback")
        self.assertTrue((self.root / "old").exists())

    def test_active_parent_or_file_preserved(self):
        for path in (self.root, self.root / "old"):
            self.assertEqual(self.run_gc(apply=True, references=[str(path)])["files"][0]["reason"], "active_reference")

    def test_unknown_live_or_unexpired_preserved(self):
        for key, value in (("kind", "backup"), ("terminal", False), ("expires_at", 21)):
            old = self.entry[key]
            self.entry[key] = value
            self.assertEqual(self.run_gc(apply=True)["files"][0]["state"], "retained")
            self.entry[key] = old

    def test_changed_recovery_refuses_before_delete(self):
        (self.root / "safe").write_text("different")
        with self.assertRaises(r.Refuse):
            self.run_gc(apply=True)
        self.assertTrue((self.root / "old").exists())

    def test_symlink_and_traversal_refused(self):
        (self.root / "link").symlink_to(self.root / "safe")
        for value in ("../outside", "/tmp/outside", "link"):
            self.entry["path"] = value
            with self.assertRaises(r.Refuse):
                self.run_gc(apply=True)

    def test_recovery_cannot_be_deletion_candidate(self):
        self.entry["recovery_path"] = "old"
        with self.assertRaises(r.Refuse):
            self.run_gc(apply=True)

    def test_budget_and_low_headroom_alert(self):
        result = self.run_gc(apply=True, max_verify_bytes=1, min_free_bytes=10**18)
        self.assertEqual(result["files"][0]["reason"], "verification_budget")
        self.assertTrue(result["headroom_alert"])
        self.assertEqual(result["verified_bytes"], 0)

    def test_aliases_cannot_delete_only_copy(self):
        for alias in ("./old", "old/", "folder//old", "folder/../old"):
            self.entry["recovery_path"] = alias
            with self.assertRaises(r.Refuse):
                self.run_gc(apply=True)
            self.assertTrue((self.root / "old").exists())

    def test_invalid_revision_and_nonfinite_expiry(self):
        for value in (None, "", "xyz", 123, int("1" * 40)):
            self.entry["revision"] = value
            with self.assertRaises(r.Refuse):
                self.run_gc(apply=True)
        self.entry["revision"] = "3" * 40
        self.entry["expires_at"] = float("nan")
        with self.assertRaises(r.Refuse):
            self.run_gc(apply=True)

    def test_cross_entry_recovery_alias_refused(self):
        second = dict(self.entry, path="./safe", recovery_path="elsewhere")
        with self.assertRaises(r.Refuse):
            r.run(self.root, {"version": 1, "artifacts": [self.entry, second]},
                  "1" * 40, "2" * 40, [], now=20, apply=True)
        self.assertTrue((self.root / "old").exists())

    def test_late_validation_failure_never_partially_deletes(self):
        second = dict(self.entry, path="other", recovery_path="missing", revision="bad")
        with self.assertRaises(r.Refuse):
            r.run(self.root, {"version": 1, "artifacts": [self.entry, second]},
                  "1" * 40, "2" * 40, [], now=20, apply=True)
        self.assertTrue((self.root / "old").exists())

    def test_held_operator_lock_refuses_apply(self):
        manifest = self.root / "manifest.json"
        manifest.write_text(json.dumps({"version": 1, "artifacts": [self.entry]}))
        lock_path = self.root / "operator.lock"
        with lock_path.open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = subprocess.run([sys.executable, str(Path(r.__file__)),
                "--root", str(self.root), "--manifest", str(manifest),
                "--lock-file", str(lock_path), "--current-revision", "1" * 40,
                "--verified-rollback-revision", "2" * 40, "--apply"],
                capture_output=True, text=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.root / "old").exists())

    def test_recovery_inode_alias_refused(self):
        (self.root / "safe").unlink()
        os.link(self.root / "old", self.root / "safe")
        with self.assertRaises(r.Refuse):
            self.run_gc(apply=True)
        self.assertTrue((self.root / "old").exists())

    def test_cross_entry_recovery_inode_alias_refused(self):
        os.link(self.root / "safe", self.root / "second")
        (self.root / "third").write_bytes(b"reproducible")
        second = dict(self.entry, path="second", recovery_path="third")
        with self.assertRaises(r.Refuse):
            r.run(self.root, {"version": 1, "artifacts": [self.entry, second]},
                  "1" * 40, "2" * 40, [], now=20, apply=True)
        self.assertTrue((self.root / "old").exists())

    def test_current_source_alias_refused(self):
        os.link(self.root / "old", self.root / "current")
        second = dict(self.entry, path="current", revision="1" * 40)
        with self.assertRaises(r.Refuse):
            r.run(self.root, {"version": 1, "artifacts": [self.entry, second]},
                  "1" * 40, "2" * 40, [], now=20, apply=True)
        self.assertTrue((self.root / "old").exists())


if __name__ == "__main__":
    unittest.main()


class LeaseSafetyTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name)
        self.lock=self.root/"lease"
        self.lock.touch(mode=0o600)

    def test_valid_lease_and_invalid_file_types_permissions_owner(self):
        from unittest.mock import patch
        with r.open_lease(self.lock) as lease:
            r.validate_lease(lease)
        self.lock.chmod(0o620)
        with self.assertRaises(r.Refuse):r.open_lease(self.lock)
        self.lock.chmod(0o600)
        with patch.object(r.os,"geteuid",return_value=os.geteuid()+1),self.assertRaises(r.Refuse):r.open_lease(self.lock)
        fifo=self.root/"fifo";os.mkfifo(fifo,0o600)
        with self.assertRaises(r.Refuse):r.open_lease(fifo)
        with self.assertRaises(r.Refuse):r.open_lease(self.root)

    def test_symlink_leaf_and_ancestor_refused(self):
        link=self.root/"link";link.symlink_to(self.lock)
        with self.assertRaises(r.Refuse):r.open_lease(link)
        directory=self.root/"alias";directory.symlink_to(self.root,target_is_directory=True)
        with self.assertRaises(r.Refuse):r.open_lease(directory/"lease")

    def test_collect_locked_revalidates_lease_before_any_manifest(self):
        from types import SimpleNamespace
        with self.lock.open("r+") as lease:
            self.lock.chmod(0o666)
            with self.assertRaises(r.Refuse):r.collect_locked(SimpleNamespace(),lease)

    def test_invalid_lease_stops_all_entrypoints_before_actions(self):
        from unittest.mock import patch
        import artifact_operation
        import artifact_schedule
        self.lock.chmod(0o666)
        results=self.root/"results";results.mkdir(mode=0o700)
        with patch.object(artifact_operation.subprocess,"Popen") as launch,self.assertRaises(ValueError):
            artifact_operation.execute(self.root,self.lock,["must-not-start"],consumer=True)
        launch.assert_not_called()
        with patch.object(artifact_schedule,"collect_locked") as collect,self.assertRaises(ValueError):
            artifact_schedule.execute(self.root/"absent-config",self.lock,results)
        collect.assert_not_called()
        argv=["retention","--root",str(self.root),"--manifest",str(self.root/"absent"),"--lock-file",str(self.lock),"--current-revision","a"*40,"--verified-rollback-revision","b"*40,"--apply"]
        with patch.object(sys,"argv",argv),patch.object(r,"collect_locked") as collect,self.assertRaises(r.Refuse):r.main()
        collect.assert_not_called()
        self.assertEqual(list(results.iterdir()),[])


class MappedReferenceTests(unittest.TestCase):
    def test_closed_descriptor_mapping_remains_a_retention_reference(self):
        # libc mmap, unlike Python mmap, does not keep a duplicate file FD.
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "mapped file\nwith newline"
            path.write_bytes(b"retained")
            child = subprocess.Popen([sys.executable, "-c", """
import ctypes, os, sys
libc = ctypes.CDLL(None, use_errno=True)
libc.mmap.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int,
                      ctypes.c_int, ctypes.c_int, ctypes.c_long]
libc.mmap.restype = ctypes.c_void_p
fd = os.open(sys.argv[1], os.O_RDONLY)
address = libc.mmap(None, 8, 1, 2, fd, 0)
if address == ctypes.c_void_p(-1).value:
    raise OSError(ctypes.get_errno(), "mmap")
os.close(fd)
print("mapped", flush=True)
sys.stdin.buffer.read(1)
""", str(path)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                stderr=subprocess.PIPE, text=True)
            try:
                import select
                self.assertTrue(select.select([child.stdout], [], [], 5)[0])
                self.assertEqual(child.stdout.readline(), "mapped\n")
                fds = Path(f"/proc/{child.pid}/fd")
                self.assertNotIn(str(path), [os.readlink(p) for p in fds.iterdir()])
                # Restrict discovery to this actual child to avoid unrelated host
                # permission policy; actual map_files/fd/readlink remain in use.
                from unittest.mock import patch
                original = Path.iterdir
                def directories(p):
                    if p == Path("/proc"):
                        return iter([Path(f"/proc/{child.pid}")])
                    return original(p)
                with patch.object(Path, "iterdir", directories):
                    refs = r.active_paths()
                self.assertIn(str(path), refs)
                self.assertTrue(r.retained_by_reference(path, refs))
                self.assertEqual(path.read_bytes(), b"retained")
            finally:
                try:
                    child.communicate("x", timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.communicate(timeout=5)

    def test_inaccessible_mapping_inventory_refuses(self):
        from unittest.mock import patch
        original = Path.iterdir
        def directories(p):
            if p == Path("/proc"):
                return iter([Path(f"/proc/{os.getpid()}")])
            if p.name == "map_files":
                raise PermissionError("mapping inventory denied")
            return original(p)
        with patch.object(Path, "iterdir", directories), self.assertRaises(r.Refuse):
            r.active_paths()

    def test_missing_mapping_directory_is_not_process_exit(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as directory:
            proc = Path(directory) / "123"
            proc.mkdir()
            with self.assertRaises(r.Refuse):
                r.mapping_links(proc)
            proc.rmdir()
            self.assertEqual(r.mapping_links(proc), [])


class MountReferenceTests(unittest.TestCase):
    def test_kernel_mount_escapes_preserve_exact_reference(self):
        from unittest.mock import patch
        cases = [
            (r'/artifacts/a\040b', '/artifacts/a b'),
            (r'/artifacts/a\011b', '/artifacts/a\tb'),
            (r'/artifacts/a\012b', '/artifacts/a\nb'),
            (r'/artifacts/a\134012b', r'/artifacts/a\012b'),
        ]
        for encoded, decoded in cases:
            with self.subTest(encoded=encoded):
                line = f'42 1 0:1 / {encoded} rw - tmpfs tmpfs rw\n'
                with patch.object(Path, 'iterdir', return_value=iter([])), \
                     patch.object(Path, 'read_text', return_value=line):
                    refs = r.active_paths()
                self.assertEqual(refs, [decoded])
                self.assertTrue(r.retained_by_reference(Path(decoded) / 'old', refs))
                self.assertFalse(r.retained_by_reference(Path(decoded + '-other') / 'old', refs))
