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
import supervised_failure as driver
from artifact_retention import Refuse

@unittest.skipUnless(os.environ.get('FC_ROOTDIRECTORY_RUNNER'),'explicit static local sandbox runner required')
class RootDirectoryProof(unittest.TestCase):
    def test_exact_existing_sandbox_acquires_bound_lease_and_exits(self):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(prefix='fc-rootdirectory-proof-',dir='/run') as tmp:
            parent=Path(tmp);root=parent/'operations';root.mkdir(mode=0o700)
            source=parent/'canonical.sqlite';source.write_bytes(b'synthetic immutable data');source.chmod(0o600)
            lease=parent/'lease';lease.touch(mode=0o600)
            runner=Path(os.environ['FC_ROOTDIRECTORY_RUNNER'])
            sha=lambda p:hashlib.sha256(p.read_bytes()).hexdigest()
            args=dict(root=root,lease=lease,source=source,source_sha256=sha(source),runner=runner,runner_sha256=sha(runner),revision='c'*40,operation='first',supervised=True,observe_supervised_failure=True,max_database_bytes=1024*1024,reserve_bytes=0)
            work=root/'first';unit=driver.unit_for(work)
            actual_run=subprocess.run
            try:
                with self.assertRaisesRegex(Refuse,'rehearsal failed'):
                    operation.execute(**args)
                import json
                proof=json.loads((work/'supervised-failure.json').read_text())
                self.assertEqual(proof['state'],'qualified_supervised_failure_evidence_only')
                self.assertEqual(proof['exit_code'],7)
                self.assertEqual(proof['supervisor']['unit'],unit)
                with lease.open('r+') as consumer:
                    fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
                self.assertEqual((work/'data/firecontroller.sqlite').read_bytes(),source.read_bytes())
                self.assertEqual(sha(work/'migration-runner'),sha(runner))
                self.assertFalse((work/'failed-equivalent-disposal.json').exists())
                print('sandbox_entries',sorted(p.name for p in work.iterdir()))
                print('sandbox_directories',{p.name:sorted(q.name for q in p.iterdir()) for p in work.iterdir() if p.is_dir()})
            finally:
                actual_run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=15)
                actual_run(['/usr/bin/systemctl','reset-failed',unit],capture_output=True,timeout=5)
                self.assertEqual(driver.properties(unit)['LoadState'],'not-found')

if __name__=='__main__':unittest.main()
