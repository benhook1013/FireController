import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import coordinator_split_adapter as adapter
from render_split_coordinator import render
from split_test_fixture import TEMPLATE, OLD_ROOT

class BoundaryTest(unittest.TestCase):
    def test_preimport_hash_refusal_does_not_execute_tampered_module(self):
        original=TEMPLATE
        main=render(original,'/root/firecontroller-operations/release-proof-new',hashlib.sha256(original).hexdigest(),OLD_ROOT).split("if __name__=='__main__':",1)[1]
        for mode in ['good','tampered','missing_closure','missing_deploy','tampered_deploy']:
            with self.subTest(mode=mode),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);marker=root/'executed'
                deploy=root/'release/tools/deploy';deploy.mkdir(parents=True)
                (deploy/'artifact_retention.py').write_text('# synthetic trusted dependency\n')
                (root/'coordinator_split_adapter.py').write_text("from pathlib import Path\nimport sys\nsys.path.insert(0,"+repr(str(deploy))+")\nimport artifact_retention\ndef run(ns, mode): Path("+repr(str(marker))+").write_text('ran')\n")
                for name in ['prepared_release.py','prepared_image_content.py']:(root/name).write_text('# synthetic closure\n')
                hashes={str(p.relative_to(root)):hashlib.sha256(p.read_bytes()).hexdigest() for p in root.rglob('*') if p.is_file()}
                if mode=='missing_closure':hashes.pop('prepared_release.py')
                if mode=='missing_deploy':hashes.pop('release/tools/deploy/artifact_retention.py')
                (root/'execution-hashes.json').write_text(json.dumps(hashes))
                (root/'execution-review.json').write_text(json.dumps({'state':'independently_reviewed','source':'source','hashes':hashes}))
                if mode=='tampered':
                    with (root/'coordinator_split_adapter.py').open('a') as out:out.write("Path("+repr(str(marker))+").write_text('BAD')\n")
                if mode=='tampered_deploy':
                    (deploy/'artifact_retention.py').write_text("from pathlib import Path\nPath("+repr(str(marker))+").write_text('BAD')\n")
                code="import sys,json,hashlib\nfrom pathlib import Path\nR=Path("+repr(tmp)+");SOURCE='source';sys.path.insert(0,str(R));sys.argv=['script','--prepare-reviewed-image']\nif True:"+main
                result=subprocess.run([sys.executable,'-I','-S','-B','-c',code],capture_output=True,timeout=5)
                self.assertEqual(result.returncode==0,mode=='good',result.stderr)
                self.assertEqual(marker.exists(),mode=='good')

    def test_content_reader_timeout_kills_inherited_lock_holder(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'lease';path.touch()
            held=path.open('r+');fcntl.flock(held,fcntl.LOCK_EX)
            code="import subprocess,sys,time;subprocess.Popen(['/usr/bin/sleep','30'],pass_fds=(int(sys.argv[1]),));time.sleep(30)"
            with self.assertRaises(subprocess.TimeoutExpired):
                adapter.read_blob([sys.executable,'-c',code,str(held.fileno())],(held.fileno(),),timeout=.1)
            held.close()
            with path.open('r+') as probe:fcntl.flock(probe,fcntl.LOCK_EX|fcntl.LOCK_NB)

if __name__=='__main__':unittest.main()
