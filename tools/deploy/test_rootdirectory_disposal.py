"""Real existing RootDirectory command with retained supervised evidence."""
import fcntl
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import rehearsal_operation as operation
import bounded_rehearsal as bounded
import supervised_failure as driver
from artifact_retention import Refuse

@unittest.skipUnless(os.environ.get('FC_ROOTDIRECTORY_RUNNER'),'explicit static local sandbox runner required')
class RootDirectoryDisposalProof(unittest.TestCase):
    def test_exact_existing_sandbox_disposes_only_equivalent_copies(self):
        self.run_case()

    def test_changed_start_preserves_every_output(self):
        self.run_case('start')

    def test_foreign_sandbox_file_refuses_disposal(self):
        self.run_case('foreign')

    def test_changed_output_refuses_disposal(self):
        self.run_case('output')

    def run_case(self,tamper=None):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(prefix='fc-rootdirectory-proof-',dir='/run') as tmp:
            parent=Path(tmp);root=parent/'operations';root.mkdir(mode=0o700)
            source=parent/'canonical.sqlite';source.write_bytes(b'synthetic immutable data');source.chmod(0o600)
            lease=parent/'lease';lease.touch(mode=0o600)
            runner=Path(os.environ['FC_ROOTDIRECTORY_RUNNER'])
            sha=lambda p:hashlib.sha256(p.read_bytes()).hexdigest()
            args=dict(root=root,lease=lease,source=source,source_sha256=sha(source),runner=runner,runner_sha256=sha(runner),revision='c'*40,operation='first',supervised=True,observe_supervised_failure=True,max_database_bytes=1024*1024,reserve_bytes=0)
            budget=parent/'budget';budget.touch(mode=0o600)
            source_lease=parent/'source-lease';source_lease.touch(mode=0o600)
            work=root/'first';unit=driver.unit_for(work)
            actual_run=subprocess.run
            original_execute=operation.execute
            def execute(**kwargs):
                try:return original_execute(**kwargs)
                except operation.KnownSupervisedFailure:
                    if tamper=='start':(work/'started.json').write_text('{}')
                    if tamper=='foreign':(work/'foreign').write_text('unknown data')
                    if tamper=='output':(work/'data/firecontroller.sqlite').write_bytes(b'changed output')
                    raise
            try:
                with patch.object(operation,'execute',side_effect=execute):
                    with self.assertRaises(Refuse):
                        bounded.execute(budget_lease=budget,source_lease=source_lease,discard_failed_equivalent=True,**args)
                if tamper:
                    self.assertTrue((work/'data/firecontroller.sqlite').exists())
                    self.assertTrue((work/'migration-runner').exists())
                    self.assertFalse((work/'supervised-disposal-intent.json').exists())
                    self.assertFalse((work/'supervised-equivalent-disposal.json').exists())
                    self.assertEqual(source.read_bytes(),b'synthetic immutable data')
                    with self.assertRaises(Refuse):bounded.admit(root)
                    return
                import json
                proof=json.loads((work/'supervised-failure.json').read_text())
                self.assertEqual(proof['state'],'qualified_supervised_failure_evidence_only')
                self.assertEqual(proof['exit_code'],7)
                self.assertEqual(proof['supervisor']['unit'],unit)
                with lease.open('r+') as consumer:
                    fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
                self.assertFalse((work/'data/firecontroller.sqlite').exists())
                self.assertEqual(source.read_bytes(),b'synthetic immutable data')
                self.assertFalse((work/'migration-runner').exists())
                self.assertEqual(sha(runner),args['runner_sha256'])
                self.assertTrue((work/'supervised-equivalent-disposal.json').exists())
                self.assertTrue((work/'supervised-disposal-intent.json').exists())
                bounded.admit(root)
                print('sandbox_entries',sorted(p.name for p in work.iterdir()))
                print('sandbox_directories',{p.name:sorted(q.name for q in p.iterdir()) for p in work.iterdir() if p.is_dir()})
            finally:
                actual_run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=15)
                actual_run(['/usr/bin/systemctl','reset-failed',unit],capture_output=True,timeout=5)
                self.assertEqual(driver.properties(unit)['LoadState'],'not-found')

if __name__=='__main__':unittest.main()
