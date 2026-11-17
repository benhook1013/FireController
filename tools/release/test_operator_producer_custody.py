"""Join offline assembly to existing producer-only custody; native build is stubbed."""
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'tools/deploy'))
import artifact_operation

def load_package():
    spec=importlib.util.spec_from_file_location('operator_package',ROOT/'tools/release/package-operator.py')
    p=importlib.util.module_from_spec(spec);spec.loader.exec_module(p);return p

def verify_custody(p,lease):
    # A fresh open must contend; the existing inherited FD must remain usable in
    # the actual subprocess shape used by the native packager.
    code="import os,fcntl,sys;s=os.fstat(int(os.environ['FC_ARTIFACT_LEASE_FD']));p=os.stat(sys.argv[1]);assert (s.st_dev,s.st_ino)==(p.st_dev,p.st_ino);f=open(sys.argv[1],'r+');\ntry:fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)\nexcept BlockingIOError:raise SystemExit(75)\nraise SystemExit(1)"
    result=subprocess.run([sys.executable,'-I','-S','-B','-c',code,str(lease)],**p.clients.artifact_lease_options())
    if result.returncode!=75:raise ValueError('producer custody not preserved')

def worker(root,lease,reader):
    p=load_package()
    def native(binary,platform,destination,revision):
        verify_custody(p,lease)
        (destination/'firecontroller-linux-amd64.tar.gz').write_bytes(b'explicit native packaging fixture')
    p.clients.package=native
    result=p.package(root,reader,root/'operator.tar.gz','a'*40)
    verify_custody(p,lease)
    if result['installation_ready']:raise ValueError('unexpected readiness')

class OperatorProducerCustody(unittest.TestCase):
    @unittest.skipUnless(os.environ.get('FC_HEALTH_HELPER'),'qualified historical reader unavailable')
    def test_existing_producer_owns_whole_assembly_without_enrollment(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent=Path(tmp);root=parent/'operation';root.mkdir(mode=0o700)
            lease=parent/'lease';lease.touch(mode=0o600)
            command=[sys.executable,'-I','-S','-B',str(Path(__file__).resolve()),'worker',str(root),str(lease),os.environ['FC_HEALTH_HELPER']]
            result=artifact_operation.execute(root,lease,command,producer_only=True)
            self.assertEqual(result,{'state':'producer_finished_unenrolled'})
            self.assertEqual([p.name for p in root.iterdir()],['operator.tar.gz'])
            import fcntl
            with lease.open('r+') as f:fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)

if __name__=='__main__':
    if len(sys.argv)>1 and sys.argv[1]=='worker':worker(Path(sys.argv[2]),Path(sys.argv[3]),Path(sys.argv[4]))
    else:unittest.main()