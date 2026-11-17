import fcntl
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
spec=importlib.util.spec_from_file_location('supervised_failure',Path(__file__).with_name('supervised_failure.py'))
# Existing imports use ordinary repo-local module resolution in this test entry.
import supervised_failure as driver
from artifact_retention import Refuse

@unittest.skipUnless(os.environ.get('FC_REHEARSAL_SERVICE_PROOF')=='1','explicit disposable service qualification')
class DriverTests(unittest.TestCase):
    def test_actual_driver_retains_bound_failed_unit_and_requires_same_custody(self):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(prefix='fc-driver-',dir='/run') as tmp:
            parent=Path(tmp);work=parent/'work';work.mkdir(mode=0o700)
            lease=parent/'lease';lease.touch(mode=0o600)
            script=parent/'runner.py';script.write_text("import fcntl,time,sys\nf=open(sys.argv[1],'r+')\nfcntl.flock(f,fcntl.LOCK_EX)\ntime.sleep(.5)\nraise SystemExit(7)")
            unit=driver.unit_for(work)
            command=['/usr/bin/systemd-run','--unit='+unit,'--wait','--pipe','--property=Type=exec','--property=KillMode=control-group','--property=RuntimeMaxSec=10','/usr/bin/python3',str(script),str(lease)]
            try:
                code,observed=driver.run_observed(work,lease,driver.ident(lease),command,timeout=5)
                self.assertEqual(code,7);self.assertIsNotNone(observed)
                self.assertTrue((work/'supervisor-start.json').exists());self.assertTrue((work/'supervisor-observed.json').exists())
                with lease.open('r+') as consumer:
                    fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
                    self.assertEqual(driver.qualify_failure(work,lease,consumer,observed,code)['state'],'known_failed_unit')
                    original_properties=driver.properties
                    def changed_group(unit):
                        current=original_properties(unit);current['ControlGroup']='/system.slice/foreign.service';return current
                    with patch.object(driver,'properties',side_effect=changed_group):
                        with self.assertRaisesRegex(Refuse,'cgroup changed'):
                            driver.qualify_failure(work,lease,consumer,observed,code)
                    with self.assertRaisesRegex(Refuse,'unknown'):
                        driver.qualify_failure(work,lease,consumer,None,code)
                    lease.rename(parent/'old-lease');lease.touch(mode=0o600)
                    with self.assertRaisesRegex(Refuse,'custody'):
                        driver.qualify_failure(work,lease,consumer,observed,code)
            finally:
                subprocess.run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=15)
                subprocess.run(['/usr/bin/systemctl','reset-failed',unit],capture_output=True,timeout=5)
                self.assertEqual(driver.properties(unit)['LoadState'],'not-found')

if __name__=='__main__':unittest.main()
