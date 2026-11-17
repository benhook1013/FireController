"""Synthetic transient systemd service proof; never changes existing units."""
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
import uuid

ROOT=Path(__file__).parent
spec=importlib.util.spec_from_file_location('bundle_fixture',ROOT/'test_bundle_offline.py')
b=importlib.util.module_from_spec(spec);spec.loader.exec_module(b)

@unittest.skipUnless(os.environ.get('FC_HEALTH_SERVICE')=='1','explicit local transient service qualification only')
class ServiceQualification(b.PackagedOfflineQualification):
    # Override inherited test: this variant never patches publisher constants or
    # calls its main in-process. It runs the extracted script's actual CLI.
    def test_pinned_old_helper_real_ownership_and_no_transport(self):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(prefix='fc-health-service-',dir='/run') as tmp:
            root=Path(tmp);publisher,binary=self.prepare_publisher(root,Path(os.environ['FC_HEALTH_HELPER']))
            varlib=root/'var-lib';varlib.mkdir()
            configs=varlib/'firecontroller/operator-offsite-v1'
            stage=varlib/'firecontroller/operator-health-staging'
            reports=varlib/'firecontroller/operator-health'
            for d in [configs,stage,reports]:d.mkdir(mode=0o700,parents=True)
            os.chown(reports,65532,65532)
            for name in ['data','managed-backups']:(varlib/'firecontroller'/name).mkdir(mode=0o700)
            instance='instance_'+'a'*32;inputs={}
            for name,label in publisher.PROVIDERS:
                d=configs/name;d.mkdir(mode=0o700);state=d/'state';state.mkdir(mode=0o700)
                # Synthetic placeholders only, for exact observed inaccessible paths.
                (d/'rclone.conf').write_text('synthetic unused fixture\n');(d/'rclone.conf').chmod(0o600)
                c={'version':1,'source_root':str(root/'source'),'instance_id':instance,'state_dir':'/var/lib/firecontroller/operator-offsite-v1/'+name+'/state','rclone_binary':str(root/'missing-rclone'),'rclone_config':str(root/'missing-rclone.conf'),'remote':'fixture:unused','keep':1,'timeout':'10m','max_bundle_bytes':1048576}
                receipt={'version':1,'id':'backup_'+'b'*32,'instance_id':instance,'snapshot_finished_at':'2026-11-17T05:00:00Z','event_sequence':1,'archive_sha256':'c'*64,'archive_bytes':100}
                binding=hashlib.sha256(json.dumps([instance,c['source_root'],c['remote'],c['rclone_config']],separators=(',',':')).encode()).hexdigest()
                ledger={'version':1,'binding':binding,'owned':[receipt],'last_success':receipt,'last_attempt':{'state':'complete','started_at':'2026-11-17T05:01:00Z','finished_at':'2026-11-17T05:02:00Z','reason':''},'retention_reason':''}
                for path,value in [(d/'offsite.json',c),(state/'offsite.json',ledger)]:
                    path.write_text(json.dumps(value));path.chmod(0o600);inputs[path]=path.read_bytes()
            command=[sys.executable,'-I','-S','-B',publisher.__file__,'--fcd',str(binary),'--sha256',b.qualification.PIN]
            done=self.launch(command,varlib)
            self.assertEqual(done.returncode,0,done.stderr)
            self.assertEqual(json.loads(done.stdout),{'published':3,'unavailable':[]})
            before={}
            for name,label in publisher.PROVIDERS:
                path=reports/(name+'.json');meta=path.stat();value=json.loads(path.read_bytes())
                self.assertEqual((meta.st_uid,meta.st_gid,stat.S_IMODE(meta.st_mode)),(65532,65532,0o600))
                self.assertEqual((value['instance_id'],value['label'],value['attempt_state']),(instance,label,'complete'))
                self.assertTrue(set(value)<=publisher.FIELDS);before[path]=path.read_bytes()
            # Exercise old launch semantics against the reports written by the
            # packaged isolated launch, then return to the packaged launch.
            # This proves only publisher/report compatibility, not application
            # database rollback or atomic service-unit switching.
            baseline={p:json.loads(body) for p,body in before.items()}
            legacy=[command[0]]+command[4:]
            for launch in [legacy,command]:
                replay=self.launch(launch,varlib)
                self.assertEqual(replay.returncode,0,replay.stderr)
                self.assertEqual(json.loads(replay.stdout),{'published':3,'unavailable':[]})
                for path,expected in baseline.items():
                    actual=json.loads(path.read_bytes())
                    self.assertEqual({k:v for k,v in actual.items() if k!='observed_at'},
                                     {k:v for k,v in expected.items() if k!='observed_at'})
                self.assertEqual({p:p.read_bytes() for p in inputs},inputs)
            before={p:p.read_bytes() for p in before}
            command[-1]='0'*64
            failed=self.launch(command,varlib)
            self.assertNotEqual(failed.returncode,0)
            self.assertEqual({p:p.read_bytes() for p in before},before)
            self.assertEqual({p:p.read_bytes() for p in inputs},inputs)
            self.assertFalse((root/'missing-rclone').exists());self.assertFalse((root/'missing-rclone.conf').exists())

    def launch(self,command,varlib):
        unit='fc-health-qualification-'+uuid.uuid4().hex+'.service'
        properties=['Type=oneshot','User=root','Group=root','UMask=0077','TimeoutStartSec=45s',
                    'PrivateTmp=yes','PrivateNetwork=yes','ProtectHome=yes','ProtectSystem=strict',
                    'NoNewPrivileges=yes','CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE',
                    'AmbientCapabilities=','RestrictAddressFamilies=AF_UNIX',
                    'ProtectKernelTunables=yes','ProtectKernelModules=yes','ProtectControlGroups=yes',
                    'ReadOnlyPaths=/var/lib/firecontroller/operator-offsite-v1/onedrive /var/lib/firecontroller/operator-offsite-v1/s3',
                    'InaccessiblePaths=/var/lib/firecontroller/operator-offsite-v1/onedrive/rclone.conf /var/lib/firecontroller/operator-offsite-v1/s3/rclone.conf /var/lib/firecontroller/data /var/lib/firecontroller/managed-backups',
                    'BindPaths='+str(varlib)+':/var/lib',
                    'ReadWritePaths=/var/lib/firecontroller/operator-health /var/lib/firecontroller/operator-health-staging']
        args=['/usr/bin/systemd-run','--quiet','--wait','--pipe','--collect','--unit='+unit]
        for value in properties:args+=['--property='+value]
        done=subprocess.run(args+command,env={'PATH':'/usr/bin:/bin'},capture_output=True,text=True,timeout=65)
        # --collect disposes both successful and failed synthetic units. No
        # persistent unit file is created, and the host /var/lib is never mounted over.
        state=subprocess.run(['/usr/bin/systemctl','show',unit,'--property=LoadState','--value'],capture_output=True,text=True,timeout=10)
        self.assertEqual(state.stdout.strip(),'not-found',state.stdout+state.stderr)
        return done

if __name__=='__main__':unittest.main()