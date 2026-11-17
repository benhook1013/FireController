"""Opt-in synthetic running-generation reconciliation, never a live service switch."""
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
import uuid

def properties(unit):
    r=subprocess.run(['/usr/bin/systemctl','show',unit,'--property=InvocationID,MainPID,ActiveState,SubState,LoadState'],capture_output=True,text=True,timeout=5)
    return dict(line.split('=',1) for line in r.stdout.splitlines() if '=' in line)

def observe(unit,expected):
    first=properties(unit)
    if first.get('ActiveState')!='active' or first.get('SubState')!='running':raise ValueError('not running')
    pid=int(first['MainPID'])
    if pid<=0 or not first.get('InvocationID'):raise ValueError('missing instance')
    args=Path('/proc',str(pid),'cmdline').read_bytes().rstrip(b'\0').split(b'\0')
    if args!=[x.encode() for x in expected]:raise ValueError('unexpected generation')
    if properties(unit)!=first:raise ValueError('changed observation')
    return {'invocation':first['InvocationID'],'pid':pid,'argv':expected}

def revalidate(unit,receipt):
    current=observe(unit,receipt['argv'])
    if current!=receipt:raise ValueError('stale invocation')

@unittest.skipUnless(os.environ.get('FC_HEALTH_RECONCILE')=='1','explicit synthetic local systemd reconciliation only')
class Reconciliation(unittest.TestCase):
    def wait(self,predicate):
        end=time.monotonic()+5
        while time.monotonic()<end:
            if predicate():return
            time.sleep(.02)
        self.fail('synthetic service did not reach expected state')
    def test_loaded_running_and_custody_are_separate_from_file_intent(self):
        self.assertEqual(os.geteuid(),0)
        with tempfile.TemporaryDirectory(dir='/run',prefix='fc-reconcile-') as tmp:
            root=Path(tmp);unit='fc-health-reconcile-'+uuid.uuid4().hex+'.service'
            lock=open(root/'operation.lock','a+b');fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            # Actual competing process cannot enter this fixture's custody interval.
            rival=subprocess.run([sys.executable,'-I','-S','-B',__file__,'lock',str(root/'operation.lock')],capture_output=True)
            self.assertEqual(rival.returncode,75)
            try:
                old=None
                for generation in ['old','new']:
                    argv=[sys.executable,'-I','-S','-B',str(Path(__file__).resolve()),'worker',str(root),generation]
                    subprocess.run(['/usr/bin/systemd-run','--quiet','--collect','--unit='+unit,'--property=Type=exec','--property=RuntimeMaxSec=10s','--property=PrivateNetwork=yes','--property=NoNewPrivileges=yes']+argv,check=True,capture_output=True,timeout=10)
                    self.wait(lambda:(root/(generation+'.ready')).exists())
                    receipt=observe(unit,argv)
                    revalidate(unit,receipt)
                    if old is not None:
                        self.assertNotEqual(receipt['invocation'],old['invocation'])
                        with self.assertRaises(ValueError):revalidate(unit,old)
                    # A desired file or prior operation receipt cannot turn this
                    # running process into another generation.
                    wrong=list(argv);wrong[-1]='unobserved'
                    with self.assertRaises(ValueError):observe(unit,wrong)
                    (root/(generation+'.release')).write_text('release')
                    self.wait(lambda:properties(unit).get('LoadState')=='not-found')
                    with self.assertRaises(ValueError):revalidate(unit,receipt)
                    old=receipt
            finally:
                # Cleanup is restricted to the random test unit, never existing units.
                if properties(unit).get('LoadState')!='not-found':
                    subprocess.run(['/usr/bin/systemctl','stop',unit],capture_output=True,timeout=10)
                self.wait(lambda:properties(unit).get('LoadState')=='not-found')
                lock.close()

if __name__=='__main__':
    if len(sys.argv)>1 and sys.argv[1]=='worker':
        root=Path(sys.argv[2]);generation=sys.argv[3]
        (root/(generation+'.ready')).write_text('ready')
        end=time.monotonic()+8
        while not (root/(generation+'.release')).exists():
            if time.monotonic()>end:raise SystemExit(74)
            time.sleep(.02)
    elif len(sys.argv)>1 and sys.argv[1]=='lock':
        with open(sys.argv[2],'a+b') as f:
            try:fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BlockingIOError:raise SystemExit(75)
    else:unittest.main()