"""Explicit root/network-isolated qualification; never uses live configs or ledgers."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import shutil
import stat
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT=pathlib.Path(__file__).parent
spec=importlib.util.spec_from_file_location('publisher',ROOT/'publish_backup_health.py')
p=importlib.util.module_from_spec(spec);spec.loader.exec_module(p)
PIN='987f8f1ad5c77c573b4f0d7de9bff15328085e406deae6246a04fc21e987596c'

@unittest.skipUnless(os.environ.get('FC_HEALTH_OFFLINE')=='1','explicit isolated root qualification only')
class OfflineQualification(unittest.TestCase):
    def prepare_publisher(self, root, original):
        binary=root/"fcd";shutil.copyfile(original,binary);binary.chmod(0o755)
        return p,binary

    def test_pinned_old_helper_real_ownership_and_no_transport(self):
        self.assertEqual(os.geteuid(),0)
        self.assertEqual(pathlib.Path('/proc/net/route').read_text().splitlines()[1:],[])
        original=pathlib.Path(os.environ['FC_HEALTH_HELPER'])
        self.assertEqual(hashlib.sha256(original.read_bytes()).hexdigest(),PIN)
        with tempfile.TemporaryDirectory(prefix='fc-health-qualification-') as tmp:
            root=pathlib.Path(tmp);p,binary=self.prepare_publisher(root,original)
            configs=root/'configs';stage=root/'stage';reports=root/'reports'
            for d in [configs,stage,reports]:d.mkdir(mode=0o700)
            os.chown(reports,65532,65532)
            inputs={}
            instance='instance_'+'a'*32
            for name,label in p.PROVIDERS:
                d=configs/name;d.mkdir(mode=0o700);state=d/'state';state.mkdir(mode=0o700)
                c={'version':1,'source_root':str(root/'source'),'instance_id':instance,'state_dir':str(state),'rclone_binary':str(root/'missing-rclone'),'rclone_config':str(root/'missing-rclone.conf'),'remote':'fixture:unused','keep':1,'timeout':'10m','max_bundle_bytes':1048576}
                receipt={'version':1,'id':'backup_'+'b'*32,'instance_id':instance,'snapshot_finished_at':'2026-11-17T05:00:00Z','event_sequence':1,'archive_sha256':'c'*64,'archive_bytes':100}
                binding=hashlib.sha256(json.dumps([instance,c['source_root'],c['remote'],c['rclone_config']],separators=(',',':')).encode()).hexdigest()
                ledger={'version':1,'binding':binding,'owned':[receipt],'last_success':receipt,'last_attempt':{'state':'complete','started_at':'2026-11-17T05:01:00Z','finished_at':'2026-11-17T05:02:00Z','reason':''},'retention_reason':''}
                for path,value in [(d/'offsite.json',c),(state/'offsite.json',ledger)]:
                    path.write_text(json.dumps(value));path.chmod(0o600);inputs[path]=path.read_bytes()
            output=io.StringIO()
            with patch.object(p,'CONFIG_ROOT',configs),patch.object(p,'STAGING',stage),patch.object(p,'REPORTS',reports),patch.object(sys,'argv',['publisher','--fcd',str(binary),'--sha256',PIN]),contextlib.redirect_stdout(output):
                self.assertEqual(p.main(),0)
            self.assertEqual(json.loads(output.getvalue()),{'published':3,'unavailable':[]})
            for name,label in p.PROVIDERS:
                final=reports/(name+'.json');meta=final.stat();value=json.loads(final.read_bytes())
                self.assertEqual((meta.st_uid,meta.st_gid,stat.S_IMODE(meta.st_mode)),(65532,65532,0o600))
                self.assertEqual(value['instance_id'],instance);self.assertEqual(value['label'],label)
                self.assertEqual(value['last_snapshot_at'],'2026-11-17T05:00:00Z')
                self.assertEqual(value['attempt_state'],'complete');self.assertEqual(value['max_age_seconds'],10800)
                self.assertTrue(set(value)<=p.FIELDS)
            report_bytes={path:path.read_bytes() for path in reports.iterdir()}
            with patch.object(p,'CONFIG_ROOT',configs),patch.object(p,'STAGING',stage),patch.object(p,'REPORTS',reports),patch.object(sys,'argv',['publisher','--fcd',str(binary),'--sha256','0'*64]):
                with self.assertRaisesRegex(RuntimeError,'Qualified helper hash mismatch'):
                    p.main()
            self.assertEqual({path:path.read_bytes() for path in reports.iterdir()},report_bytes)
            self.assertEqual({path:path.read_bytes() for path in inputs},inputs)
            self.assertFalse((root/'missing-rclone').exists());self.assertFalse((root/'missing-rclone.conf').exists())
            self.assertEqual(sorted(x.name for x in reports.iterdir()),['google.json','onedrive.json','s3.json'])

if __name__=='__main__':unittest.main()