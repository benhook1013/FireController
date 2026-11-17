import fcntl
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import signal
import subprocess
import time
import sys
import tempfile
import unittest
from unittest.mock import patch

import rehearsal_operation as operation
from artifact_retention import Refuse


class RehearsalOperationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.root = self.base / "operations"
        self.root.mkdir(mode=0o700)
        self.lease = self.base / "lease"
        self.lease.touch(mode=0o600)
        self.source = self.base / "recovery.sqlite"
        with sqlite3.connect(self.source) as db:
            db.execute("CREATE TABLE retained(value TEXT NOT NULL)")
            db.execute("INSERT INTO retained VALUES('original')")
        self.source.chmod(0o600)
        self.original = self.source.read_bytes()
        self.runner = self.base / "runner"
        self.code = '''import argparse,hashlib,json,os,sqlite3
p=argparse.ArgumentParser();p.add_argument('--data-dir');p.add_argument('--report');a=p.parse_args()
assert int(os.environ['FC_ARTIFACT_LEASE_FD']) >= 3
os.fstat(int(os.environ['FC_ARTIFACT_LEASE_FD']))
with sqlite3.connect(a.data_dir+'/firecontroller.sqlite') as db:
 rows=db.execute('SELECT value FROM retained ORDER BY value').fetchall()
 assert rows == [('original',)]
 db.execute('PRAGMA user_version=7')
report={'state':'passed','source_revision':'c'*40,'harness':'candidate-specific-schema-init-v1',
'existing_table_counts_unchanged':True,'existing_table_logical_digests_unchanged':True,
'provider_workers_constructed':False,'tables':{'retained':{'rows':len(rows),'sha256':hashlib.sha256(json.dumps(rows).encode()).hexdigest()}}}
fd=os.open(a.report,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'w') as f:json.dump(report,f)
'''
        self.set_runner(self.code)
        self.args = dict(root=self.root, lease=self.lease, source=self.source,
                         source_sha256=hashlib.sha256(self.original).hexdigest(),
                         runner=self.runner, runner_sha256=self.hash(self.runner), revision="c"*40,
                         operation="first", max_database_bytes=1024**2, reserve_bytes=0)

    @staticmethod
    def hash(path):
        return hashlib.sha256(path.read_bytes()).hexdigest()

    def set_runner(self, code):
        self.runner.write_text('#!' + sys.executable + '\n' + code)
        self.runner.chmod(0o700)
        if hasattr(self, 'args'):
            self.args['runner_sha256'] = self.hash(self.runner)

    def run_operation(self, **changes):
        return operation.execute(**dict(self.args, **changes))

    def test_disposable_success_preserves_source_runner_and_proof(self):
        runner_bytes = self.runner.read_bytes()
        result = self.run_operation(discard_passed_output=True)
        work = self.root/'first'
        self.assertEqual(result['state'], 'passed')
        self.assertFalse((work/'data/firecontroller.sqlite').exists())
        self.assertEqual(self.source.read_bytes(), self.original)
        self.assertEqual(self.runner.read_bytes(), runner_bytes)
        self.assertTrue((work/'terminal.json').exists())
        self.assertTrue((work/'rehearsal.json').exists())
        proof = json.loads((work/'disposal.json').read_text())
        self.assertFalse(proof['byte_equivalent_recovery'])
        self.assertGreater(proof['logical_bytes_discarded'], 0)
        self.run_operation(operation='second', discard_passed_output=True)
        self.assertFalse((self.root/'second/data/firecontroller.sqlite').exists())
        self.assertEqual(json.loads((self.root/'second/disposal.json').read_text())['logical_tables_sha256'], proof['logical_tables_sha256'])

    def test_disposable_failure_preserves_diagnostic_database(self):
        self.set_runner(self.code + '\nraise SystemExit(1)\n')
        with self.assertRaisesRegex(Refuse, 'rehearsal failed'):
            self.run_operation(discard_passed_output=True)
        work = self.root/'first'
        self.assertTrue((work/'data/firecontroller.sqlite').exists())
        self.assertFalse((work/'disposal.json').exists())
        self.assertEqual(self.source.read_bytes(), self.original)

    def test_disposable_mode_does_not_adopt_old_work(self):
        self.run_operation()
        output = self.root/'first/data/firecontroller.sqlite'
        before = output.read_bytes()
        with self.assertRaises(FileExistsError):
            self.run_operation(discard_passed_output=True)
        self.assertEqual(output.read_bytes(), before)

    def test_mutating_consumer_and_fresh_recovery_replay(self):
        self.assertEqual(self.run_operation(), {'state':'passed','operation':'first','enrolled':False})
        first = json.loads((self.root/'first/terminal.json').read_text())
        output = self.root/'first/data/firecontroller.sqlite'
        self.assertNotEqual(output.read_bytes(), self.original)
        self.assertFalse(first['byte_equivalent_recovery'])
        self.assertEqual(first['output_sha256'], self.hash(output))
        self.assertEqual(first['source_sha256'], self.args['source_sha256'])
        self.assertEqual(first['runner_sha256'], self.args['runner_sha256'])
        self.assertEqual(first['lease_identity'], [self.lease.stat().st_dev,self.lease.stat().st_ino])
        self.assertEqual(self.source.read_bytes(), self.original)
        first_bytes = output.read_bytes()
        self.run_operation(operation='fresh-recovery')
        second = json.loads((self.root/'fresh-recovery/terminal.json').read_text())
        self.assertEqual(first['logical_tables_sha256'], second['logical_tables_sha256'])
        self.assertEqual(output.read_bytes(), first_bytes)
        with sqlite3.connect(self.root/'fresh-recovery/data/firecontroller.sqlite') as db:
            self.assertEqual(db.execute('SELECT value FROM retained').fetchall(), [('original',)])
            self.assertEqual(db.execute('PRAGMA user_version').fetchone(), (7,))
        # Original recovery and every output/receipt retained; no collector is invoked.
        self.assertEqual(self.source.read_bytes(), self.original)
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ['first','fresh-recovery'])

    def test_supervised_handoff_same_lease_and_original_isolation(self):
        actual_run = subprocess.run
        def service(command, **options):
            self.assertEqual(command[0], '/usr/bin/systemd-run')
            self.assertEqual(options['timeout'], 210)
            for prop in ('Type=exec','CPUQuota=100%','MemoryMax=512M','MemorySwapMax=0',
                         'RuntimeMaxSec=180','TimeoutStopSec=5','KillMode=control-group',
                         'PrivateNetwork=yes','PrivateDevices=yes','MountAPIVFS=yes','NoNewPrivileges=yes'):
                self.assertIn('--property='+prop, command)
            work=self.root/'first'
            self.assertIn('--property=RootDirectory='+str(work),command)
            self.assertIn('--property=BindPaths='+str(self.lease)+':/artifact.lease',command)
            self.assertEqual(command[command.index('--lease-device')+1],str(self.lease.stat().st_dev))
            self.assertEqual(command[command.index('--lease-inode')+1],str(self.lease.stat().st_ino))
            self.assertFalse((work/'terminal.json').exists())
            # Model only service dispatch: real same-host lease and foreground SQLite
            # consumer run here; this is not a RootDirectory/systemd execution test.
            with self.lease.open() as child_lease:
                fcntl.flock(child_lease,fcntl.LOCK_EX|fcntl.LOCK_NB)
                with self.lease.open() as other:
                    with self.assertRaises(BlockingIOError):
                        fcntl.flock(other,fcntl.LOCK_EX|fcntl.LOCK_NB)
                return actual_run([str(work/'migration-runner'),'--data-dir',str(work/'data'),
                                   '--report',str(work/'rehearsal.json')],
                                  pass_fds=(child_lease.fileno(),),
                                  env=dict(os.environ,FC_ARTIFACT_LEASE_FD=str(child_lease.fileno())),
                                  stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
        with patch.object(operation.subprocess,'run',side_effect=service):
            self.run_operation(supervised=True)
        self.assertTrue((self.root/'first/terminal.json').exists())
        self.assertEqual(self.source.read_bytes(),self.original)

    def test_supervisor_failure_or_uncertain_client_no_terminal(self):
        for index, result in enumerate((subprocess.CompletedProcess([],1),subprocess.TimeoutExpired('systemd-run',210))):
            with self.subTest(index=index):
                kwargs={'side_effect':result} if isinstance(result,Exception) else {'return_value':result}
                with patch.object(operation.subprocess,'run',**kwargs), self.assertRaises(Refuse):
                    self.run_operation(supervised=True,operation='service'+str(index))
                self.assertFalse((self.root/('service'+str(index))/'terminal.json').exists())
                self.assertTrue((self.root/('service'+str(index))/'started.json').exists())
        self.assertEqual(self.source.read_bytes(),self.original)

    def test_supervised_fixed_deadline_and_literal_paths(self):
        with self.assertRaisesRegex(Refuse,'deadline is fixed'):
            self.run_operation(supervised=True,timeout=999)
        for bad in ('/tmp/a:b','/tmp/a b','/tmp/%n'):
            with self.assertRaisesRegex(Refuse,'literal systemd-safe'):
                operation.supervised_command(Path(bad),self.lease,self.lease.stat())
        self.assertEqual(list(self.root.iterdir()),[])

    def test_wrapper_crash_keeps_child_lease_and_no_terminal(self):
        self.set_runner(self.code + "\nfrom pathlib import Path;import time;"
                        "Path(a.data_dir).parent.joinpath('runner.pid').write_text(str(os.getpid()));time.sleep(20)\n")
        command = [sys.executable, str(Path(operation.__file__))]
        for key in ('root','lease','source','source_sha256','runner','runner_sha256','revision','operation'):
            command.extend(['--'+key.replace('_','-'),str(self.args[key])])
        wrapper = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        child = None
        try:
            ready = self.root/'first/runner.pid'
            deadline = time.monotonic()+5
            while not ready.exists() and wrapper.poll() is None and time.monotonic()<deadline:
                time.sleep(.01)
            self.assertTrue(ready.exists())
            child = int(ready.read_text())
            wrapper.kill();wrapper.wait(timeout=3)
            with self.lease.open() as held:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.assertFalse((self.root/'first/terminal.json').exists())
            self.assertEqual(self.source.read_bytes(),self.original)
        finally:
            if wrapper.poll() is None:
                wrapper.kill();wrapper.wait(timeout=3)
            if child is not None:
                try:os.killpg(child,signal.SIGKILL)
                except ProcessLookupError:pass

    def test_foreground_exit_with_descendant_never_terminal(self):
        self.set_runner(self.code + "\nimport subprocess;from pathlib import Path;"
                        "Path(a.data_dir).parent.joinpath('group').write_text(str(os.getpgrp()));"
                        "subprocess.Popen(["+repr(sys.executable)+",'-c','import time;time.sleep(20)'],"
                        "pass_fds=(int(os.environ['FC_ARTIFACT_LEASE_FD']),))\n")
        try:
            with self.assertRaisesRegex(Refuse,'left descendants'):
                self.run_operation()
            self.assertFalse((self.root/'first/terminal.json').exists())
            with self.lease.open() as held:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
        finally:
            group=self.root/'first/group'
            if group.exists():
                try:os.killpg(int(group.read_text()),signal.SIGKILL)
                except ProcessLookupError:pass

    def test_failure_and_crash_identity_never_adopted(self):
        self.set_runner('raise SystemExit(1)\n')
        with self.assertRaisesRegex(Refuse, 'rehearsal failed'):
            self.run_operation()
        self.assertTrue((self.root/'first/started.json').exists())
        self.assertFalse((self.root/'first/terminal.json').exists())
        with self.assertRaises(FileExistsError):
            self.run_operation()
        self.assertEqual(self.source.read_bytes(), self.original)

    def test_deadline_never_terminal(self):
        self.set_runner('import time;time.sleep(20)\n')
        with self.assertRaisesRegex(Refuse, 'deadline exceeded'):
            self.run_operation(timeout=.05)
        self.assertFalse((self.root/'first/terminal.json').exists())
        with self.lease.open() as lease:
            fcntl.flock(lease, fcntl.LOCK_EX|fcntl.LOCK_NB)

    def test_lease_contention_before_any_new_output(self):
        with self.lease.open() as held:
            fcntl.flock(held, fcntl.LOCK_SH|fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                self.run_operation()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_actual_runner_holds_exclusive_lease(self):
        self.set_runner("import fcntl;f=open(" + repr(str(self.lease)) + ", 'r+')\n"
                        "try:fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)\n"
                        "except BlockingIOError:pass\nelse:raise AssertionError('lease absent')\n" + self.code)
        self.run_operation()

    def test_wrong_report_revision_or_proof_has_no_terminal(self):
        for index, replacement in enumerate(("report['source_revision']='d'*40", "report['provider_workers_constructed']=True", "report['existing_table_counts_unchanged']=1", "report['tables']={}")):
            self.set_runner(self.code.replace("fd=os.open", replacement + "\nfd=os.open"))
            with self.subTest(replacement=replacement), self.assertRaises(Refuse):
                self.run_operation(operation='bad'+str(index))
            self.assertFalse((self.root/('bad'+str(index))/'terminal.json').exists())

    def test_mutated_recovery_or_runner_refuses_terminal(self):
        for index, target in enumerate((self.source,self.runner)):
            self.set_runner(self.code + "\nwith open("+repr(str(target))+",'ab') as f:f.write(b'changed')\n")
            with self.subTest(target=target.name), self.assertRaisesRegex(Refuse, 'identity mismatch'):
                self.run_operation(operation='mutation'+str(index))
            self.assertFalse((self.root/('mutation'+str(index))/'terminal.json').exists())
            self.source.write_bytes(self.original)

    def test_inputs_alias_paths_or_stale_hash_refused_before_copy(self):
        for changes in ({'source_sha256':'0'*64}, {'runner_sha256':'0'*64}, {'operation':'../escape'}, {'revision':'short'}, {'timeout':float('nan')}, {'max_database_bytes':True}):
            with self.subTest(changes=changes), self.assertRaises(Refuse):
                self.run_operation(**changes)
        alias=self.base/'alias';alias.symlink_to(self.source)
        with self.assertRaisesRegex(Refuse, 'canonical'):
            self.run_operation(source=alias)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_source_sidecars_and_headroom_refuse_before_copy(self):
        sidecar=Path(str(self.source)+'-wal');sidecar.touch()
        with self.assertRaisesRegex(Refuse, 'standalone'):
            self.run_operation()
        sidecar.unlink()
        with patch.object(operation.os, 'statvfs') as fs:
            fs.return_value.f_bavail=0;fs.return_value.f_frsize=4096
            with self.assertRaisesRegex(Refuse, 'headroom'):
                self.run_operation()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_post_run_sidecar_and_symlink_report_refuse(self):
        for index, extra in enumerate(("open(a.data_dir+'/firecontroller.sqlite-wal','w').close()", "os.unlink(a.report);os.symlink("+repr(str(self.source))+",a.report)")):
            self.set_runner(self.code+'\n'+extra+'\n')
            with self.subTest(extra=extra), self.assertRaises(Refuse):
                self.run_operation(operation='output'+str(index))
            self.assertFalse((self.root/('output'+str(index))/'terminal.json').exists())


if __name__ == '__main__':
    unittest.main()
