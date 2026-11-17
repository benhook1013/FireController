"""Real process-exit proof for the isolated split, not host power loss."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

CHILD = r'''
import os,sys
from pathlib import Path
import prepared_release as p
root=Path(sys.argv[1]); mode=sys.argv[2]; boundary=sys.argv[3]
identity={'source':'synthetic-frozen','inputs':'exact'}
def verify(fd):return {'uid':'same','generation':1}
def event(value):
 with (root/'events').open('a') as out:out.write(value+'\n');out.flush();os.fsync(out.fileno())
def build(fd):
 event('build');(root/'image-manifest.json').write_text('immutable image')
 if boundary=='producer_return':os._exit(73)
def rollout(fd):event('rollout')
original=p.durable_new
def durable(path,value):
 original(path,value)
 if path.name==boundary:os._exit(73)
p.durable_new=durable
try:
 if mode=='prepare':p.prepare(root,root/'release.lock',identity,verify,build)
 else:p.admit(root,root/'release.lock',identity,verify,lambda:None,rollout)
except (ValueError,FileExistsError,FileNotFoundError):sys.exit(19)
'''


class CrashTest(unittest.TestCase):
    def run_child(self, root, mode, boundary='none'):
        return subprocess.run([sys.executable,'-B','-c',CHILD,str(root),mode,boundary],
                              cwd=Path(__file__).parent,timeout=5).returncode

    def test_reconstructs_only_complete_preparation_and_never_replays_started_action(self):
        for boundary in ['preparation-started.json','producer_return','prepared-image.json','admission-started.json']:
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);(root/'release.lock').touch(mode=0o600)
                if boundary=='admission-started.json':
                    self.assertEqual(self.run_child(root,'prepare'),0)
                    self.assertEqual(self.run_child(root,'admit',boundary),73)
                else:
                    self.assertEqual(self.run_child(root,'prepare',boundary),73)
                before={p.name:p.read_bytes() for p in root.iterdir()}
                self.assertEqual(self.run_child(root,'prepare'),19)
                self.assertEqual({p.name:p.read_bytes() for p in root.iterdir()},before)
                if boundary=='prepared-image.json':
                    self.assertEqual(self.run_child(root,'admit'),0)
                    self.assertEqual((root/'events').read_text().splitlines(),['build','rollout'])
                else:
                    self.assertEqual(self.run_child(root,'admit'),19)
                    self.assertEqual({p.name:p.read_bytes() for p in root.iterdir()},before)
                final={p.name:p.read_bytes() for p in root.iterdir()}
                self.assertEqual(self.run_child(root,'admit'),19)
                self.assertEqual({p.name:p.read_bytes() for p in root.iterdir()},final)

if __name__=='__main__':unittest.main()
