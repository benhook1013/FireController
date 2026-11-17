import hashlib
import fcntl
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import coordinator_split_adapter as adapter

class AdapterTest(unittest.TestCase):
    def test_existing_call_contract_prepare_defer_admit_and_content_loss(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);lease=root/'release.lock';lease.touch(mode=0o600)
            (root/'image-artifact.lock').touch(mode=0o600)
            helpers=root/'release/tools/deploy';helpers.mkdir(parents=True)
            original=Path(__file__).parent
            for name in ['artifact_retention.py']:
                shutil.copyfile(original/name,helpers/name)
            (root/'release/bin').mkdir();(root/'release/bin/fcd').write_bytes(b'synthetic binary')
            (root/'canonical-proof.json').write_text(json.dumps({'state':'passed','source':'source'}))
            names=['release/bin/fcd','release/tools/deploy/artifact_retention.py','canonical-proof.json']
            hashes={n:hashlib.sha256((root/n).read_bytes()).hexdigest() for n in names}
            (root/'execution-hashes.json').write_text(json.dumps(hashes))
            (root/'execution-review.json').write_text(json.dumps({'state':'independently_reviewed','source':'source','hashes':hashes}))
            blobs={};events=[];ready=[False]
            def blob(raw):
                digest='sha256:'+hashlib.sha256(raw).hexdigest();blobs[digest]=raw
                return {'digest':digest,'size':len(raw)}
            layer=blob(b'prepared layer')
            cfg=blob(json.dumps({'config':{'Labels':{'org.opencontainers.image.revision':'source'}}}).encode())
            manifest=blob(json.dumps({'schemaVersion':2,'config':cfg,'layers':[layer]}).encode())
            def call(args,**kw):
                if args[0]=='python3':
                    self.assertEqual(kw['timeout'],180);self.assertEqual(len(kw['fds']),2)
                    events.append('image')
                    (root/'image.tar').write_bytes(b'prepared archive')
                    report=dict(source='source',binary_sha256=hashes['release/bin/fcd'],previous_image='old',archive_sha256=hashlib.sha256(b'prepared archive').hexdigest(),unchanged_runtime_config=True,source_revision_label_only=True,added_layer_files=['fcd'],image='localhost/firecontroller@'+manifest['digest'])
                    (root/'image-manifest.json').write_text(json.dumps(report))
                    return subprocess.CompletedProcess(args,0,'ok','')
                self.assertEqual(args,['systemctl','is-active','firecontroller-offsite-batch.timer'])
                return subprocess.CompletedProcess(args,0,'active\n','')
            def window():
                events.append('window')
                if not ready[0]:raise ValueError('original20m window closed')
            def rollout(path,**kw):
                assert_artifact_held()
                self.assertEqual(Path(path),root/'rollout.py');events.append('rollout')
                (root/'live-proof.json').write_text(json.dumps({'source':'source','state':'verified_deployed'}))
            ns=dict(R=root,RELEASE_LEASE=lease,SOURCE='source',IMAGE='old',call=call,window=window,
                    preimage=lambda:{'metadata':{'uid':'same','generation':1,'annotations':{'deployment.kubernetes.io/revision':'334'}},'spec':{'exact':'original'}})
            def content(args,fds,timeout=30):
                assert_artifact_held()
                self.assertEqual(args[:6],['k3s','ctr','-n','k8s.io','content','get'])
                self.assertEqual(timeout,30);self.assertEqual(len(fds),2)
                return blobs[args[-1]]
            def assert_artifact_held():
                with (root/'image-artifact.lock').open('r+') as competing:
                    with self.assertRaises(BlockingIOError):
                        fcntl.flock(competing,fcntl.LOCK_EX|fcntl.LOCK_NB)
            with patch.object(adapter,'read_blob',content),patch.object(adapter.runpy,'run_path',rollout):
                adapter.run(ns,'--prepare-reviewed-image')
                self.assertEqual(events,['image'])
                with (root/'image-artifact.lock').open('r+') as competing:
                    fcntl.flock(competing,fcntl.LOCK_EX|fcntl.LOCK_NB)
                    with self.assertRaises(BlockingIOError):adapter.run(ns,'--admit-reviewed-operation')
                self.assertFalse((root/'admission-started.json').exists())
                with self.assertRaises(ValueError):adapter.run(ns,'--admit-reviewed-operation')
                self.assertFalse((root/'admission-started.json').exists())
                saved=blobs.pop(layer['digest']);ready[0]=True
                with self.assertRaises(KeyError):adapter.run(ns,'--admit-reviewed-operation')
                self.assertFalse((root/'admission-started.json').exists())
                blobs[layer['digest']]=saved
                adapter.run(ns,'--admit-reviewed-operation')
                self.assertEqual(events,['image','window','window','rollout'])
                with self.assertRaises(FileExistsError):adapter.run(ns,'--admit-reviewed-operation')
                self.assertEqual(events.count('image'),1);self.assertEqual(events.count('rollout'),1)

if __name__=='__main__':unittest.main()
