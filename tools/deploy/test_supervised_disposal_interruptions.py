"""Real child exits and injected I/O errors in default-off sandbox disposal."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import bounded_rehearsal as bounded
import rehearsal_operation as operation
import supervised_equivalent as disposal
import supervised_failure as supervisor
from artifact_retention import Refuse

@unittest.skipUnless(os.environ.get('FC_ROOTDIRECTORY_RUNNER'),'explicit static disposable runner required')
class InterruptedSupervisedDisposal(unittest.TestCase):
    def test_real_exits_preserve_recovery_and_fence_incomplete_receipts(self):
        for point in ('intent_after','db_unlinked','runner_unlinked','receipt_after'):
            with self.subTest(point=point):self.run_case(point,73)
    def test_sync_and_receipt_errors_preserve_recovery_and_fence(self):
        for point in ('sync_db','sync_runner','receipt_write'):
            with self.subTest(point=point):self.run_case(point,75)
    def run_case(self,point,expected_code):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(prefix='fc-disposal-interrupt-',dir='/run') as tmp:
            parent=Path(tmp);root=parent/'operations';root.mkdir(mode=0o700)
            source=parent/'source';original=b'immutable synthetic recovery';source.write_bytes(original);source.chmod(0o600)
            runner=Path(os.environ['FC_ROOTDIRECTORY_RUNNER']);runner_hash=hashlib.sha256(runner.read_bytes()).hexdigest()
            for name in ('lease','source-lease','budget'):(parent/name).touch(mode=0o600)
            args={'root':str(root),'lease':str(parent/'lease'),'source':str(source),'source_sha256':hashlib.sha256(original).hexdigest(),
                'runner':str(runner),'runner_sha256':runner_hash,'revision':'c'*40,'operation':'first','supervised':True,
                'observe_supervised_failure':True,'discard_failed_equivalent':True,'max_database_bytes':1024*1024,'reserve_bytes':0,
                'source_lease':str(parent/'source-lease'),'budget_lease':str(parent/'budget')}
            config=parent/'args.json';config.write_text(json.dumps(args));work=root/'first';unit=supervisor.unit_for(work)
            try:
                result=subprocess.run([sys.executable,__file__,'child',str(config),point],capture_output=True,text=True,timeout=20)
                self.assertEqual(result.returncode,expected_code,result.stderr)
                self.assertEqual(source.read_bytes(),original)
                self.assertEqual(hashlib.sha256(runner.read_bytes()).hexdigest(),runner_hash)
                for name in ('started.json','supervised-failure.json','supervisor-start.json','supervisor-observed.json',disposal.INTENT):
                    self.assertTrue((work/name).is_file(),name)
                expected_db=point=='intent_after'
                expected_runner=point in ('intent_after','db_unlinked','sync_db')
                self.assertEqual((work/'data/firecontroller.sqlite').exists(),expected_db)
                self.assertEqual((work/'migration-runner').exists(),expected_runner)
                if expected_db:self.assertEqual((work/'data/firecontroller.sqlite').read_bytes(),original)
                if expected_runner:self.assertEqual(hashlib.sha256((work/'migration-runner').read_bytes()).hexdigest(),runner_hash)
                if point=='receipt_after':bounded.admit(root)
                else:
                    self.assertFalse((work/disposal.RECEIPT).exists())
                    with self.assertRaises(Refuse):bounded.admit(root)
            finally:
                subprocess.run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=15)
                subprocess.run(['/usr/bin/systemctl','reset-failed',unit],capture_output=True,timeout=5)
                self.assertEqual(supervisor.properties(unit)['LoadState'],'not-found')

def child(args,point):
    real_json=operation.new_json;real_unlink=Path.unlink;real_sync=disposal.sync
    sync_count=0
    def record(path,body):
        if point=='receipt_write' and path.name==disposal.RECEIPT:raise OSError('injected receipt write failure')
        real_json(path,body)
        if point=='intent_after' and path.name==disposal.INTENT:os._exit(73)
        if point=='receipt_after' and path.name==disposal.RECEIPT:os._exit(73)
    def unlink(path,*a,**kw):
        real_unlink(path,*a,**kw)
        if point=='db_unlinked' and path.name=='firecontroller.sqlite':os._exit(73)
        if point=='runner_unlinked' and path.name=='migration-runner':os._exit(73)
    def sync(path):
        nonlocal sync_count
        sync_count+=1
        if (point=='sync_db' and sync_count==1) or (point=='sync_runner' and sync_count==2):raise OSError('injected directory sync failure')
        real_sync(path)
    try:
        with patch.object(operation,'new_json',record),patch.object(Path,'unlink',unlink),patch.object(disposal,'sync',sync):
            bounded.execute(**args)
    except OSError:raise SystemExit(75)
    except Refuse:raise SystemExit(42)

if __name__=='__main__':
    if len(sys.argv)>1 and sys.argv[1]=='child':child(json.loads(Path(sys.argv[2]).read_text()),sys.argv[3])
    else:unittest.main()
