"""Test-only durable supervisor binding. No cleanup or production caller."""
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
import unittest
import uuid
spec=importlib.util.spec_from_file_location('supervised_fixture',Path(__file__).with_name('test_supervised_failure_evidence.py'))
model=importlib.util.module_from_spec(spec);spec.loader.exec_module(model)


def ident(path):
    s=path.lstat();return [s.st_dev,s.st_ino]

def persist(path, value):
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'w') as out:json.dump(value,out,sort_keys=True);out.flush();os.fsync(out.fileno())
    fd=os.open(path.parent,os.O_RDONLY|os.O_DIRECTORY)
    try:os.fsync(fd)
    finally:os.close(fd)

def qualify(root):
    path=root/'observed.json';s=path.lstat()
    if not stat.S_ISREG(s.st_mode) or s.st_size>8192 or s.st_uid!=os.geteuid() or s.st_mode&0o077:raise ValueError('unsafe binding')
    record=json.loads(path.read_text())
    if set(record)!={'version','root','root_identity','lease_identity','unit','invocation','cgroup'} or record['version']!=1:raise ValueError('binding schema')
    if record['root']!=str(root) or record['root_identity']!=ident(root):raise ValueError('wrong operation')
    expected='fc-rehearsal-durable-'+hashlib.sha256(str(root).encode()).hexdigest()[:24]+'.service'
    if record['unit']!=expected or record['cgroup']!='/system.slice/'+expected:raise ValueError('wrong unit binding')
    lease=root/'lease'
    if ident(lease)!=record['lease_identity']:raise ValueError('lease replaced')
    # Outer lock stays held across terminal observation plus final identity check.
    # flock uses a second FD here only in the inner helper; release outer lock
    # would be needed by that fixture, so inline its manager observation instead.
    with lease.open('r+') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        opened=os.fstat(lock.fileno())
        if [opened.st_dev,opened.st_ino]!=record['lease_identity']:raise ValueError('opened lease changed')
        first=model.show(expected)
        required={'LoadState':'loaded','ActiveState':'failed','SubState':'failed','InvocationID':record['invocation'],
            'MainPID':'0','ControlPID':'0','Result':'exit-code','ExecMainStatus':'7'}
        if not record['invocation'] or any(first.get(k)!=v for k,v in required.items()):raise ValueError('unknown terminal outcome')
        group=Path('/sys/fs/cgroup')/record['cgroup'].lstrip('/')
        if group.exists() and dict(x.split() for x in (group/'cgroup.events').read_text().splitlines()).get('populated')!='0':raise ValueError('populated')
        if model.show(expected)!=first or ident(lease)!=record['lease_identity'] or ident(root)!=record['root_identity']:raise ValueError('changed custody')
        return record

@unittest.skipUnless(os.environ.get('FC_REHEARSAL_SERVICE_PROOF')=='1','explicit disposable local service proof only')
class DurableEvidence(unittest.TestCase):
    def test_disk_only_child_revalidates_and_rejects_replaced_lease(self):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(prefix='fc-durable-',dir='/run') as tmp:
            root=Path(tmp);lease=root/'lease';lease.touch(mode=0o600)
            unit='fc-rehearsal-durable-'+hashlib.sha256(str(root).encode()).hexdigest()[:24]+'.service'
            script=root/'runner.py';script.write_text("import time;time.sleep(.6);raise SystemExit(7)")
            try:
                subprocess.run(['/usr/bin/systemd-run','--quiet','--unit='+unit,'--property=Type=exec','--property=RuntimeMaxSec=10','/usr/bin/python3',str(script)],check=True,timeout=5)
                active=model.show(unit);self.assertEqual(active['ActiveState'],'active')
                persist(root/'observed.json',{'version':1,'root':str(root),'root_identity':ident(root),'lease_identity':ident(lease),
                    'unit':unit,'invocation':active['InvocationID'],'cgroup':active['ControlGroup']})
                end=time.monotonic()+5
                while model.show(unit)['ActiveState']!='failed' and time.monotonic()<end:time.sleep(.02)
                command=[sys.executable,'-I','-S','-B',__file__,'qualify',str(root)]
                self.assertEqual(subprocess.run(command,capture_output=True,timeout=5).returncode,0)
                before=(root/'observed.json').read_bytes()
                lease.rename(root/'original-lease');lease.touch(mode=0o600)
                self.assertNotEqual(subprocess.run(command,capture_output=True,timeout=5).returncode,0)
                self.assertEqual((root/'observed.json').read_bytes(),before)
            finally:
                subprocess.run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=15)
                subprocess.run(['/usr/bin/systemctl','reset-failed',unit],capture_output=True,timeout=5)
                self.assertEqual(model.show(unit)['LoadState'],'not-found')

if __name__=='__main__':
    if len(sys.argv)>1 and sys.argv[1]=='qualify':qualify(Path(sys.argv[2]))
    else:unittest.main()
