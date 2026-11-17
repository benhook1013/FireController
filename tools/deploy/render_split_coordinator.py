"""Render only a fresh coordinator; never edit/adopt the reviewed live stage."""
import hashlib
from pathlib import Path
import re


def render(original, fresh_root, expected_sha256, old_root):
    if hashlib.sha256(original).hexdigest()!=expected_sha256 or not re.fullmatch(r'[0-9a-f]{64}',expected_sha256):
        raise ValueError('unqualified coordinator template')
    if not re.fullmatch(r'/root/firecontroller-operations/release-[a-z0-9-]+',fresh_root) or fresh_root==old_root:
        raise ValueError('fresh canonical operation root required')
    text=original.decode()
    prefix,main=text.split("if __name__=='__main__':",1)
    assert prefix.count(old_root)==1
    # Preserve the exact reviewed call timeout/reaping, preimage and natural
    # window functions. Only operation root and explicitly reviewed entry change.
    prefix=prefix.replace(old_root,fresh_root)
    return prefix+'''if __name__=='__main__':
 assert len(sys.argv)==2, 'explicit split operation mode required'
 review=json.loads((R/'execution-review.json').read_text())
 hashes=json.loads((R/'execution-hashes.json').read_text())
 assert review['state']=='independently_reviewed' and review['source']==SOURCE and review['hashes']==hashes
 assert {'coordinator_split_adapter.py','prepared_release.py','prepared_image_content.py','release/tools/deploy/artifact_retention.py'} <= set(hashes)
 for name,digest in hashes.items():
  target=R/name
  assert not target.is_symlink() and target.is_file() and hashlib.sha256(target.read_bytes()).hexdigest()==digest, name
 from coordinator_split_adapter import run
 run(globals(),sys.argv[1])
'''

if __name__=='__main__':
    raise SystemExit('Library renderer only; full fresh package/manifest review required')
