"""Test-only drop-in file transaction. No installer or service-manager caller."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

OLD=b'[Service]\nExecStart=\nExecStart=/usr/bin/python3 /old/publisher --fcd /old/reader\n'
NEW=b'[Service]\nExecStart=\nExecStart=/usr/bin/python3 -I -S -B /new/publisher --fcd /old/reader\n'

def digest(body):return hashlib.sha256(body).hexdigest()
def sync(path):
    fd=os.open(path,os.O_RDONLY|os.O_DIRECTORY)
    try:os.fsync(fd)
    finally:os.close(fd)
def create(path,body):
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'wb') as f:f.write(body);f.flush();os.fsync(f.fileno())
def regular(path):
    import stat
    s=path.lstat()
    if not stat.S_ISREG(s.st_mode) or s.st_size>65536:raise ValueError('unexpected file')
    return path.read_bytes()
def checkpoint(name):
    if os.environ.get('FC_SWITCH_CRASH')==name:os._exit(73)

def prepare(root):
    # Fixture parent is private; caller holds sole writer custody throughout.
    old=regular(root/'current.conf')
    if old!=OLD:raise ValueError('unexpected preimage')
    if (root/'operation').exists():raise ValueError('existing operation')
    stage=root/'operation';stage.mkdir(mode=0o700)
    create(stage/'old',old);create(stage/'new',NEW)
    create(stage/'intent.json',json.dumps({'version':1,'old':digest(old),'new':digest(NEW)},sort_keys=True).encode())
    sync(stage);sync(root);checkpoint('prepared')

def read_intent(root):
    stage=root/'operation'
    if set(p.name for p in stage.iterdir())!={'old','new','intent.json'}:raise ValueError('foreign operation entry')
    intent=json.loads(regular(stage/'intent.json'))
    old,new=regular(stage/'old'),regular(stage/'new')
    if intent!={'version':1,'old':digest(old),'new':digest(new)} or old!=OLD or new!=NEW:raise ValueError('invalid intent')
    return old,new

def replace_known(root,expected,target,label):
    # No systemd reload/stop/start occurs here. Atomicity is file visibility only.
    if regular(root/'current.conf')!=expected:raise ValueError('current changed')
    temp=root/(label+'.tmp')
    if temp.exists():raise ValueError('ambiguous temporary file')
    create(temp,target);sync(root);checkpoint(label+'-staged')
    if regular(root/'current.conf')!=expected:raise ValueError('current changed')
    os.replace(temp,root/'current.conf');checkpoint(label+'-replaced');sync(root)

def act(root,action):
    if action=='prepare':prepare(root);return
    old,new=read_intent(root)
    current=regular(root/'current.conf')
    if action=='switch':
        if current==new:return
        replace_known(root,old,new,'switch')
    elif action=='rollback':
        if current==old:return
        replace_known(root,new,old,'rollback')
    else:raise ValueError('unknown action')

class AtomicDropinFixture(unittest.TestCase):
    def run_child(self,root,action,crash=None):
        env={'PATH':'/usr/bin:/bin'}
        if crash:env['FC_SWITCH_CRASH']=crash
        return subprocess.run([sys.executable,'-I','-S','-B',__file__,'child',str(root),action],env=env,capture_output=True,timeout=10)
    def fixture(self,root):
        create(root/'current.conf',OLD)
        create(root/'unrelated.conf',b'unchanged original service settings')
    def test_switch_and_rollback_preserve_unrelated_settings(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);self.fixture(root);act(root,'prepare');act(root,'switch')
            self.assertEqual(regular(root/'current.conf'),NEW)
            act(root,'rollback');act(root,'rollback')
            self.assertEqual(regular(root/'current.conf'),OLD)
            self.assertEqual(regular(root/'unrelated.conf'),b'unchanged original service settings')
    def test_actual_process_exit_after_atomic_switch_recovers_from_disk_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);self.fixture(root)
            self.assertEqual(self.run_child(root,'prepare','prepared').returncode,73)
            self.assertEqual(regular(root/'current.conf'),OLD)
            self.assertEqual(self.run_child(root,'switch','switch-replaced').returncode,73)
            self.assertEqual(regular(root/'current.conf'),NEW)
            self.assertEqual(self.run_child(root,'rollback').returncode,0)
            self.assertEqual(regular(root/'current.conf'),OLD)
    def test_pre_replace_crash_leaves_old_and_fences_ambiguous_stage(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);self.fixture(root);act(root,'prepare')
            self.assertEqual(self.run_child(root,'switch','switch-staged').returncode,73)
            self.assertEqual(regular(root/'current.conf'),OLD)
            self.assertNotEqual(self.run_child(root,'switch').returncode,0)
            self.assertEqual(regular(root/'switch.tmp'),NEW)
    def test_unknown_edit_and_corrupt_intent_are_preserved(self):
        for corrupt in [False,True]:
            with self.subTest(corrupt=corrupt),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);self.fixture(root);act(root,'prepare');act(root,'switch')
                if corrupt:(root/'operation'/'intent.json').write_text('{}')
                else:(root/'current.conf').write_bytes(b'new external edit')
                before={str(p.relative_to(root)):p.read_bytes() for p in root.rglob('*') if p.is_file()}
                self.assertNotEqual(self.run_child(root,'rollback').returncode,0)
                self.assertEqual({str(p.relative_to(root)):p.read_bytes() for p in root.rglob('*') if p.is_file()},before)

if __name__=='__main__':
    if len(sys.argv)>1 and sys.argv[1]=='child':act(Path(sys.argv[2]),sys.argv[3])
    else:unittest.main()