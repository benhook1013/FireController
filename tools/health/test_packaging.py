import importlib.util
import hashlib
import json
import pathlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT=pathlib.Path(__file__).parent
spec=importlib.util.spec_from_file_location('publisher',ROOT/'publish_backup_health.py')
p=importlib.util.module_from_spec(spec);spec.loader.exec_module(p)

class PackagingTest(unittest.TestCase):
    def test_source_is_exact_observed_publisher(self):
        self.assertEqual(hashlib.sha256((ROOT/'publish_backup_health.py').read_bytes()).hexdigest(),'d7b097884fef19d102b69485d92d35c355f3e3fddd6d080793a0a79e24eb32b7')

    def test_deployment_description_preserves_separate_old_reader(self):
        d=json.loads((ROOT/'observed-layout.json').read_text())
        self.assertFalse(d['installation_ready'])
        self.assertEqual(d['helper_sha256'],'987f8f1ad5c77c573b4f0d7de9bff15328085e406deae6246a04fc21e987596c')
        self.assertIn('/0bcc2ac13377c6344fafe3ead1483904e08accbe/',d['helper'])
        self.assertEqual(d['config_root'],str(p.CONFIG_ROOT))
        self.assertEqual(d['staging'],str(p.STAGING))
        self.assertEqual(d['reports'],str(p.REPORTS))

    def exercise(self,returncode=0,extra=None,final_instance=None):
        with tempfile.TemporaryDirectory() as temp:
            root=pathlib.Path(temp); configs=root/'configs'; staging=root/'stage'; reports=root/'reports'
            (configs/'s3').mkdir(parents=True);staging.mkdir();reports.mkdir()
            instance='instance_'+'a'*32
            config=configs/'s3'/'offsite.json'
            config.write_text(json.dumps({'instance_id':instance,'state_dir':str(configs/'s3'/'state')}));config.chmod(0o600)
            value={'version':1,'instance_id':instance,'label':'S3','max_age_seconds':10800}
            previous={**value,'instance_id':final_instance or instance}
            final=reports/'s3.json';final.write_text(json.dumps(previous));final.chmod(0o600)
            original=final.read_bytes();calls=[]
            actual_regular=p.regular
            def regular(path,uid):
                # Real type/mode checks, caller ownership mapped to fixture user.
                return actual_regular(path,path.lstat().st_uid)
            def run(argv,**kwargs):
                calls.append((argv,kwargs))
                result=dict(value)
                if extra:result.update(extra)
                (staging/'s3.json').write_text(json.dumps(result));(staging/'s3.json').chmod(0o600)
                return subprocess.CompletedProcess(argv,returncode)
            with patch.object(p,'CONFIG_ROOT',configs),patch.object(p,'STAGING',staging),patch.object(p,'REPORTS',reports),patch.object(p,'regular',regular),patch.object(p.subprocess,'run',run),patch.object(p.os,'fchown'):
                if returncode or extra or final_instance:
                    with self.assertRaises(RuntimeError):p.publish(pathlib.Path('/qualified/old-fcd'),'s3','S3')
                    self.assertEqual(final.read_bytes(),original)
                else:
                    p.publish(pathlib.Path('/qualified/old-fcd'),'s3','S3')
                    self.assertEqual(json.loads(final.read_bytes()),value)
            self.assertEqual(sorted(x.name for x in reports.iterdir()),['s3.json'])
            if final_instance:self.assertEqual(calls,[]);return
            self.assertEqual(len(calls),1)
            argv,kwargs=calls[0]
            self.assertEqual(argv,['/qualified/old-fcd','backup-offsite-status','--config',str(config),'--health-file',str(staging/'s3.json'),'--health-label','S3','--health-max-age','3h'])
            self.assertEqual(kwargs,{'stdout':subprocess.DEVNULL,'stderr':subprocess.DEVNULL,'timeout':15,'check':False})

    def test_success_only_status_command_and_atomic_report(self):self.exercise()
    def test_helper_failure_retains_previous(self):self.exercise(returncode=1)
    def test_extra_disclosure_retains_previous(self):self.exercise(extra={'secret':'not allowed'})
    def test_previous_instance_refused_before_helper(self):self.exercise(final_instance='instance_'+'b'*32)

if __name__=='__main__':unittest.main()