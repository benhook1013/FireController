import fcntl
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import rehearsal_operation as operation
import supervised_failure as supervisor
import supervised_rehearsal_handoff as handoff
from artifact_retention import Refuse

@unittest.skipUnless(os.environ.get('FC_REHEARSAL_SERVICE_PROOF')=='1','explicit disposable producer handoff proof')
class HandoffTests(unittest.TestCase):
    def test_actual_child_takes_released_lease_then_bound_evidence_preserves_output(self):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(prefix='fc-handoff-',dir='/run') as tmp:
            parent=Path(tmp);root=parent/'operations';root.mkdir(mode=0o700)
            work=root/'new';work.mkdir(mode=0o700);(work/'data').mkdir(mode=0o700)
            source=parent/'source';source.write_bytes(b'preserved equivalent fixture');source.chmod(0o600)
            output=work/'data/firecontroller.sqlite';output.write_bytes(source.read_bytes());output.chmod(0o600)
            lease=parent/'lease';lease.touch(mode=0o600)
            runner=parent/'runner.py';runner.write_text("import fcntl,time,sys\nf=open(sys.argv[1],'r+')\nfcntl.flock(f,fcntl.LOCK_EX)\ntime.sleep(.5)\nraise SystemExit(7)");runner.chmod(0o700)
            sha=lambda p:hashlib.sha256(p.read_bytes()).hexdigest()
            args=dict(root=root,operation='new',lease=lease,source=source,source_sha256=sha(source),runner=runner,runner_sha256=sha(runner),revision='c'*40)
            started={'version':1,'kind':'replayable-rehearsal-not-enrolled','root':str(root),'operation':'new',
                'source_path':str(source),'source_sha256':sha(source),'runner_path':str(runner),'runner_sha256':sha(runner),
                'source_revision':'c'*40,'lease_identity':supervisor.ident(lease)}
            operation.new_json(work/'started.json',started)
            unit=supervisor.unit_for(work)
            command=['/usr/bin/systemd-run','--unit='+unit,'--wait','--pipe','--property=Type=exec','--property=RuntimeMaxSec=10','--property=KillMode=control-group','/usr/bin/python3',str(runner),str(lease)]
            try:
                with lease.open('r+') as consumer:
                    fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
                    proof=handoff.observe_failed_handoff(args,consumer,command)
                    self.assertEqual(proof['state'],'qualified_supervised_failure_evidence_only')
                    self.assertEqual(proof['supervisor']['exit_code'],7)
                    with lease.open('r+') as rival:
                        with self.assertRaises(BlockingIOError):fcntl.flock(rival,fcntl.LOCK_EX|fcntl.LOCK_NB)
                    with self.assertRaises(Refuse):handoff.observe_failed_handoff(args,consumer,command)
                self.assertEqual(output.read_bytes(),source.read_bytes())
                self.assertFalse((work/'failed-equivalent-disposal.json').exists())
            finally:
                subprocess.run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=15)
                subprocess.run(['/usr/bin/systemctl','reset-failed',unit],capture_output=True,timeout=5)
                self.assertEqual(supervisor.properties(unit)['LoadState'],'not-found')

if __name__=='__main__':unittest.main()
