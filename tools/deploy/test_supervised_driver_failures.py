"""Real service failure/descendant cases for unactivated supervisor driver."""
import fcntl
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import supervised_failure as driver
from artifact_retention import Refuse

@unittest.skipUnless(os.environ.get('FC_REHEARSAL_SERVICE_PROOF')=='1','explicit disposable service tests')
class DriverFailures(unittest.TestCase):
    def setUp(self):
        self.assertEqual(os.geteuid(),0)
        self.tmp=tempfile.TemporaryDirectory(prefix='fc-supervised-failure-',dir='/run')
        self.parent=Path(self.tmp.name);self.work=self.parent/'work';self.work.mkdir(mode=0o700)
        self.lease=self.parent/'lease';self.lease.touch(mode=0o600)
        self.unit=driver.unit_for(self.work)
    def tearDown(self):
        subprocess.run(['/usr/bin/systemctl','stop',self.unit],capture_output=True,timeout=15)
        subprocess.run(['/usr/bin/systemctl','reset-failed',self.unit],capture_output=True,timeout=5)
        self.assertEqual(driver.properties(self.unit)['LoadState'],'not-found')
        self.tmp.cleanup()
    def command(self,body):
        script=self.parent/'runner.py';script.write_text(body)
        return ['/usr/bin/systemd-run','--unit='+self.unit,'--wait','--pipe','--property=Type=exec','--property=KillMode=control-group','--property=TimeoutStopSec=1','--property=RuntimeMaxSec=10','/usr/bin/python3',str(script),str(self.lease),str(self.parent)]
    def test_client_deadline_never_returns_qualified_failure(self):
        command=self.command('import time;time.sleep(4);raise SystemExit(7)')
        with self.assertRaisesRegex(Refuse,'service unresolved'):
            driver.run_observed(self.work,self.lease,driver.ident(self.lease),command,timeout=.1)
        self.assertTrue((self.work/'supervisor-start.json').exists())
        self.assertFalse((self.work/'failure.json').exists())
        self.assertFalse((self.work/'failed-equivalent-disposal.json').exists())
    def test_observation_error_reaps_owned_client_without_qualifying_service(self):
        command=self.command('import time;time.sleep(4);raise SystemExit(7)')
        actual_properties=driver.properties;actual_popen=subprocess.Popen;children=[]
        def launch(*a,**kw):
            child=actual_popen(*a,**kw)
            if a[0][0]=='/usr/bin/systemd-run':children.append(child)
            return child
        def observe(unit):
            if children:raise RuntimeError('injected observation failure')
            return actual_properties(unit)
        with patch.object(driver.subprocess,'Popen',side_effect=launch),patch.object(driver,'properties',side_effect=observe):
            with self.assertRaisesRegex(RuntimeError,'observation'):
                driver.run_observed(self.work,self.lease,driver.ident(self.lease),command,timeout=5)
        self.assertEqual(len(children),1);self.assertIsNotNone(children[0].poll())
        self.assertFalse((self.work/'supervisor-observed.json').exists())

    def test_main_failure_does_not_qualify_until_descendant_cgroup_stopped(self):
        command=self.command("import fcntl,os,sys,time\nfrom pathlib import Path\nf=open(sys.argv[1],'r+')\nfcntl.flock(f,fcntl.LOCK_EX)\npid=os.fork()\nif pid==0:\n os.setsid()\n Path(sys.argv[2]+'/descendant.pid').write_text(str(os.getpid()))\n time.sleep(8)\n os._exit(0)\ntime.sleep(.4)\nraise SystemExit(7)\n")
        code,observed=driver.run_observed(self.work,self.lease,driver.ident(self.lease),command,timeout=5)
        self.assertEqual(code,7);self.assertIsNotNone(observed)
        descendant=int((self.parent/'descendant.pid').read_text())
        # setsid escapes the leader process group, not the unit's cgroup.
        proc=Path('/proc')/str(descendant)
        if proc.exists():
            # A dead zombie holds no descriptors and is not a running writer.
            self.assertEqual((proc/'stat').read_text().split(') ',1)[1].split()[0],'Z')
        with self.lease.open('r+') as consumer:
            fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.assertEqual(driver.qualify_failure(self.work,self.lease,consumer,observed,code)['state'],'known_failed_unit')

if __name__=='__main__':unittest.main()
