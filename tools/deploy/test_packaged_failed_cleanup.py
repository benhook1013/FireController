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
import prepare_rehearsal as prepare
import sys
import json
import supervised_failure as driver
from artifact_retention import Refuse

@unittest.skipUnless(os.environ.get('FC_ROOTDIRECTORY_RUNNER'),'explicit static local sandbox runner required')
class PackagedRootDirectoryProof(unittest.TestCase):
    def test_exact_existing_sandbox_disposes_only_equivalent_copies(self):
        self.run_case()

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
            package=parent/'helper-package';package.mkdir(mode=0o700)
            hashes={}
            for name in prepare.FAILED_HELPERS|{'prepare_rehearsal.py','prepare_successor.py'}:
                body=(Path(prepare.__file__).parent/name).read_bytes()
                (package/name).write_bytes(body);hashes[name]=hashlib.sha256(body).hexdigest()
            (package/'manifest.json').write_text(json.dumps({'sha256':hashes,'kind':'isolated-qualified-helper-package'}))
            policy=dict(root=str(root),budget_lease=str(budget),source_lease=str(source_lease),source_lease_uid=0,source_owner_uid=0,lease=str(lease))
            config={name:str(args[name]) for name in prepare.INPUTS};config.update(mode='bounded',failed_equivalent_cleanup=True)
            entry=parent/'rehearse.py';entry.write_text(prepare.render(config,policy,package))
            try:
                result=actual_run([sys.executable,'-I','-S','-B',str(entry)],env={'PATH':'/usr/bin:/bin'},capture_output=True,text=True,timeout=20)
                self.assertNotEqual(result.returncode,0)
                self.assertIn('rehearsal failed',result.stderr)
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
            finally:
                actual_run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=15)
                actual_run(['/usr/bin/systemctl','reset-failed',unit],capture_output=True,timeout=5)
                self.assertEqual(driver.properties(unit)['LoadState'],'not-found')

if __name__=='__main__':unittest.main()
