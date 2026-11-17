"""Real fixed-path publisher CLI under explicit private mount/network namespaces."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest

ROOT=Path(__file__).parent
spec=importlib.util.spec_from_file_location('bundle_fixture',ROOT/'test_bundle_offline.py')
b=importlib.util.module_from_spec(spec);spec.loader.exec_module(b)

@unittest.skipUnless(os.environ.get('FC_HEALTH_FIXED_PATHS')=='1','explicit private mount/network qualification only')
class FixedPathQualification(b.PackagedOfflineQualification):
    # Override inherited test: this variant never patches publisher constants or
    # calls its main in-process. It runs the extracted script's actual CLI.
    def test_pinned_old_helper_real_ownership_and_no_transport(self):
        self.assertEqual(os.geteuid(),0)
        self.assertNotEqual(os.readlink('/proc/self/ns/mnt'),os.readlink('/proc/1/ns/mnt'))
        self.assertEqual(Path('/proc/net/route').read_text().splitlines()[1:],[])
        subprocess.run(['/bin/mount','--make-rprivate','/'],check=True)
        subprocess.run(['/bin/mount','-t','tmpfs','-o','mode=0755,size=8m','health-fixture','/var/lib'],check=True)
        with tempfile.TemporaryDirectory(prefix='fc-health-fixed-') as tmp:
            root=Path(tmp);publisher,binary=self.prepare_publisher(root,Path(os.environ['FC_HEALTH_HELPER']))
            configs=Path('/var/lib/firecontroller/operator-offsite-v1')
            stage=Path('/var/lib/firecontroller/operator-health-staging')
            reports=Path('/var/lib/firecontroller/operator-health')
            for d in [configs,stage,reports]:d.mkdir(mode=0o700,parents=True)
            os.chown(reports,65532,65532)
            instance='instance_'+'a'*32;inputs={}
            for name,label in publisher.PROVIDERS:
                d=configs/name;d.mkdir(mode=0o700);state=d/'state';state.mkdir(mode=0o700)
                c={'version':1,'source_root':str(root/'source'),'instance_id':instance,'state_dir':str(state),'rclone_binary':str(root/'missing-rclone'),'rclone_config':str(root/'missing-rclone.conf'),'remote':'fixture:unused','keep':1,'timeout':'10m','max_bundle_bytes':1048576}
                receipt={'version':1,'id':'backup_'+'b'*32,'instance_id':instance,'snapshot_finished_at':'2026-11-17T05:00:00Z','event_sequence':1,'archive_sha256':'c'*64,'archive_bytes':100}
                binding=hashlib.sha256(json.dumps([instance,c['source_root'],c['remote'],c['rclone_config']],separators=(',',':')).encode()).hexdigest()
                ledger={'version':1,'binding':binding,'owned':[receipt],'last_success':receipt,'last_attempt':{'state':'complete','started_at':'2026-11-17T05:01:00Z','finished_at':'2026-11-17T05:02:00Z','reason':''},'retention_reason':''}
                for path,value in [(d/'offsite.json',c),(state/'offsite.json',ledger)]:
                    path.write_text(json.dumps(value));path.chmod(0o600);inputs[path]=path.read_bytes()
            command=[sys.executable,'-I','-S','-B',publisher.__file__,'--fcd',str(binary),'--sha256',b.qualification.PIN]
            done=subprocess.run(command,env={'PATH':'/usr/bin:/bin'},capture_output=True,text=True,timeout=60)
            self.assertEqual(done.returncode,0,done.stderr)
            self.assertEqual(json.loads(done.stdout),{'published':3,'unavailable':[]})
            before={}
            for name,label in publisher.PROVIDERS:
                path=reports/(name+'.json');meta=path.stat();value=json.loads(path.read_bytes())
                self.assertEqual((meta.st_uid,meta.st_gid,stat.S_IMODE(meta.st_mode)),(65532,65532,0o600))
                self.assertEqual((value['instance_id'],value['label'],value['attempt_state']),(instance,label,'complete'))
                self.assertTrue(set(value)<=publisher.FIELDS);before[path]=path.read_bytes()
            command[-1]='0'*64
            failed=subprocess.run(command,env={'PATH':'/usr/bin:/bin'},capture_output=True,text=True,timeout=60)
            self.assertNotEqual(failed.returncode,0)
            self.assertEqual({p:p.read_bytes() for p in before},before)
            self.assertEqual({p:p.read_bytes() for p in inputs},inputs)
            self.assertFalse((root/'missing-rclone').exists());self.assertFalse((root/'missing-rclone.conf').exists())

if __name__=='__main__':unittest.main()