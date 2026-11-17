import importlib.util
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
spec=importlib.util.spec_from_file_location('service_override',Path(__file__).with_name('service_override.py'))
s=importlib.util.module_from_spec(spec);spec.loader.exec_module(s)

class ServiceOverrideTest(unittest.TestCase):
    def test_exact_exec_only_no_settings_or_timer_changes(self):
        body=s.render('/opt/firecontroller/releases/qualified/health')
        self.assertEqual(body.splitlines()[:2],['[Service]','ExecStart='])
        self.assertEqual(len(body.splitlines()),3)
        self.assertIn('/usr/bin/python3 -I -S -B ',body)
        self.assertIn('--sha256 '+s.READER_SHA,body)
        self.assertNotIn('backup-push',body)

    def test_unsafe_or_ambiguous_path_refused(self):
        for path in ['/','//opt/x','relative','/opt/../x','/opt//x','/opt/x/','/opt/a b','/opt/%h','/opt/$HOME','/opt/x\nUser=bad','/opt/a\\b']:
            with self.subTest(path=path),self.assertRaises(ValueError):s.render(path)

    @unittest.skipUnless(shutil.which('systemd-analyze'),'systemd analyzer unavailable')
    def test_systemd_accepts_override_with_observed_service_properties(self):
        # Static validation only. It does not start a service or prove its namespace.
        with tempfile.TemporaryDirectory() as tmp:
            unit=Path(tmp)/'firecontroller-health-qualification.service'
            unit.write_text('[Unit]\nDescription=Isolated health launch qualification\n[Service]\nType=oneshot\nUser=root\nGroup=root\nUMask=0077\nTimeoutStartSec=45s\nPrivateTmp=yes\nProtectHome=yes\nProtectSystem=strict\nNoNewPrivileges=yes\nExecStart=/usr/bin/false\n'+s.render('/opt/firecontroller/releases/qualified/health'))
            done=subprocess.run(['systemd-analyze','verify',str(unit)],capture_output=True,text=True,timeout=30)
            self.assertEqual(done.returncode,0,done.stderr)

if __name__=='__main__':unittest.main()