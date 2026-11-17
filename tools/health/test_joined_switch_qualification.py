"""Join file-switch and real transient generation observations; test-only."""
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import uuid
import fcntl

def load(name,file):
    spec=importlib.util.spec_from_file_location(name,Path(__file__).with_name(file))
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
r=load('service_reconciliation','test_service_reconciliation.py')
tx=load('switch_protocol','test_switch_protocol.py')

@unittest.skipUnless(os.environ.get('FC_HEALTH_RECONCILE')=='1','explicit synthetic systemd integration only')
class JoinedQualification(r.Reconciliation):
    def test_file_switch_does_not_claim_running_switch_or_rollback(self):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(dir='/run',prefix='fc-switch-joined-') as tmp:
            root=Path(tmp);unit='fc-health-reconcile-'+uuid.uuid4().hex+'.service'
            lock=open(root/'operation.lock','a+b');fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            tx.create(root/'current.conf',tx.OLD);tx.prepare(root)
            def argv(generation):
                return [sys.executable,'-I','-S','-B',str(Path(r.__file__).resolve()),'worker',str(root),generation]
            def expected():
                body=tx.regular(root/'current.conf')
                if body==tx.OLD:return argv('old')
                if body==tx.NEW:return argv('new')
                raise ValueError('unknown drop-in')
            def start(generation):
                command=expected();self.assertEqual(command,argv(generation))
                subprocess.run(['/usr/bin/systemd-run','--quiet','--collect','--unit='+unit,'--property=Type=exec','--property=RuntimeMaxSec=10s','--property=PrivateNetwork=yes','--property=NoNewPrivileges=yes']+command,check=True,capture_output=True,timeout=10)
                self.wait(lambda:(root/(generation+'.ready')).exists())
                return r.observe(unit,command)
            def finish(generation):
                (root/(generation+'.release')).write_text('release')
                self.wait(lambda:r.properties(unit).get('LoadState')=='not-found')
            try:
                old=start('old')
                tx.act(root,'switch')
                with self.assertRaises(ValueError):r.observe(unit,expected())
                r.revalidate(unit,old) # Old process is still real despite new file.
                finish('old')
                new=start('new')
                with self.assertRaises(ValueError):r.revalidate(unit,old)
                tx.act(root,'rollback')
                self.assertEqual(tx.regular(root/'current.conf'),tx.OLD)
                with self.assertRaises(ValueError):r.observe(unit,expected())
                r.revalidate(unit,new) # File rollback is not process rollback.
                finish('new')
                (root/'old.ready').unlink();(root/'old.release').unlink()
                restored=start('old')
                self.assertNotEqual(restored['invocation'],old['invocation'])
                self.assertNotEqual(restored['invocation'],new['invocation'])
                r.revalidate(unit,restored)
                finish('old')
            finally:
                if r.properties(unit).get('LoadState')!='not-found':subprocess.run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=10)
                self.wait(lambda:r.properties(unit).get('LoadState')=='not-found')
                lock.close()

if __name__=='__main__':unittest.main()