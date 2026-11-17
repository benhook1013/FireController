import hashlib
import tempfile
from pathlib import Path
import unittest
from deployment_duplicates import collect, dependency_paths
import json
import os
import time
from artifact_retention import Refuse


class DuplicatesTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name)
        (self.root/"active").mkdir();(self.root/"rollback").mkdir()
        self.op=self.root/"completed"; self.op.mkdir()
        self.nested=self.op/"rehearsal"; self.nested.mkdir()
        self.marker=self.op/"terminal.json"; self.marker.write_text("completed")
        self.a=self.op/"canonical.sqlite"; self.a.write_bytes(b"database")
        self.b=self.nested/"firecontroller.sqlite"; self.b.write_bytes(b"database")
        self.c={"root":str(self.root),"protected_roots":[str(self.root/"active"),str(self.root/"rollback")],"terminal_roots":[{"path":str(self.op),"marker":str(self.marker),"marker_sha256":hashlib.sha256(b"completed").hexdigest()}]}

    def test_dryrun_then_apply_then_repeat(self):
        self.assertEqual(collect(self.c,[])["files_eligible"],1); self.assertTrue(self.b.exists())
        self.assertEqual(collect(self.c,[],True)["files_removed"],1)
        self.assertEqual(self.a.read_bytes(),b"database"); self.assertFalse(self.b.exists())
        self.assertEqual(collect(self.c,[],True)["files_removed"],0)

    def test_unique_retained(self):
        self.b.write_bytes(b"unique!!")
        self.assertEqual(collect(self.c,[],True)["files_removed"],0)
        self.assertTrue(self.a.exists());self.assertTrue(self.b.exists())

    def test_active_reference_retained(self):
        self.assertEqual(collect(self.c,[str(self.b)],True)["files_removed"],0)

    def test_sqlite_sidecar_retained(self):
        Path(str(self.b)+"-wal").touch()
        self.assertEqual(collect(self.c,[],True)["files_removed"],0)

    def test_protected_root_refuses(self):
        self.c["protected_roots"].append(str(self.op))
        with self.assertRaises(Refuse):collect(self.c,[],True)

    def test_changed_marker_refuses(self):
        self.marker.write_text("changed")
        with self.assertRaises(Refuse):collect(self.c,[],True)

    def test_symlink_refuses(self):
        (self.op/"link").symlink_to(self.a)
        with self.assertRaises(Refuse):collect(self.c,[],True)

    def test_budget_does_not_expand(self):
        self.c["max_verify_bytes"]=len(b"completed")+1
        self.assertEqual(collect(self.c,[],True)["files_removed"],0)

    def test_overlapping_enrollment_refuses(self):
        self.c["terminal_roots"]*=2
        with self.assertRaises(Refuse):collect(self.c,[],True)

    def test_indirect_configuration_dependency_is_retained(self):
        child=self.root/"provider.json"; child.write_text(json.dumps({"rclone_binary":str(self.b)})); child.chmod(0o600)
        top=self.root/"batch.json"; top.write_text(json.dumps({"configs":[str(child)]})); top.chmod(0o600)
        refs=dependency_paths([str(top)])
        self.assertIn(str(self.b),refs)
        self.assertEqual(collect(self.c,list(refs),True)["files_removed"],0)

    def test_missing_indirect_dependency_refuses_before_cleanup(self):
        child=self.root/"provider.json"; child.write_text(json.dumps({"rclone_binary":str(self.root/"missing")})); child.chmod(0o600)
        top=self.root/"batch.json"; top.write_text(json.dumps({"configs":[str(child)]})); top.chmod(0o600)
        with self.assertRaises(Refuse):dependency_paths([str(top)])
        self.assertTrue(self.b.exists())

    def test_dependency_cycle_terminates(self):
        top=self.root/"batch.json"; top.write_text(json.dumps({"self":str(top)})); top.chmod(0o600)
        self.assertEqual(dependency_paths([str(top)]),{str(top)})

    def test_equal_and_ancestor_directory_references_retain(self):
        for ref in [self.op,self.root,self.nested]:
            self.assertEqual(collect(self.c,[str(ref)],True)["files_removed"],0)
            self.assertTrue(self.b.exists())

    def test_missing_or_file_protection_refuses(self):
        for path in [self.root/"missing",self.a]:
            self.c["protected_roots"]=[str(path),str(self.root/"rollback")]
            with self.assertRaises(Refuse):collect(self.c,[],True)
            self.assertTrue(self.b.exists())

    def test_provider_json_without_json_suffix_is_traversed(self):
        for name in ["provider.conf","extensionless"]:
            child=self.root/name; child.write_text(json.dumps({"rclone_binary":str(self.root/"missing")}));child.chmod(0o600)
            top=self.root/"batch.json";top.write_text(json.dumps({"configs":[str(child)]}));top.chmod(0o600)
            with self.assertRaises(Refuse):dependency_paths([str(top)])

    def test_budget_includes_marker_and_final_reverification(self):
        self.c["max_verify_bytes"]=len(b"completed")+2*len(b"database")
        result=collect(self.c,[],True)
        self.assertEqual(result["files_removed"],0)
        self.assertEqual(result["verified_bytes"],self.c["max_verify_bytes"])
        self.c["max_verify_bytes"]=len(b"completed")+4*len(b"database")
        result=collect(self.c,[],True)
        self.assertEqual(result["files_removed"],1)
        self.assertEqual(result["verified_bytes"],self.c["max_verify_bytes"])

    def test_fifo_marker_and_config_refuse_promptly(self):
        fifo=self.op/"fifo";os.mkfifo(fifo)
        self.c["terminal_roots"][0]["marker"]=str(fifo)
        started=time.monotonic()
        with self.assertRaises(Refuse):collect(self.c,[],True)
        with self.assertRaises(Refuse):dependency_paths([str(fifo)])
        self.assertLess(time.monotonic()-started,1)
