import hashlib,json,os,tempfile,unittest
from pathlib import Path
import lifecycle_package as p
class PackageTests(unittest.TestCase):
 def fixture(self,root):
  hashes={}
  for name in p.FILES:
   data=b'# not executed\n';(root/name).write_bytes(data);hashes[name]=hashlib.sha256(data).hexdigest()
  return dict(failed_cleanup_default=False,installation_path='/installed/templates',original_operation='/retained/original',previous_template_source='a'*40,source='b'*40,sha256=hashes,rehearsal_policy=dict(budget_lease='/locks/budget',lease='/locks/consumer',root='/rehearsals',source_lease='/locks/phase',source_lease_uid=65532,source_owner_uid=65532))
 def write(self,root,m):
  b=json.dumps(m).encode();(root/'manifest.json').write_bytes(b);return hashlib.sha256(b).hexdigest()
 def test_real_bytes_without_execution_or_closure_claim(self):
  with tempfile.TemporaryDirectory() as t:
   root=Path(t);m=self.fixture(root);pin=self.write(root,m);out=p.inspect(t,pin)
   self.assertEqual(out['verified_files'],13);self.assertTrue(out['package_bytes_verified']);self.assertFalse(out['installation_ready']);self.assertFalse(out['closure_verified']);self.assertEqual(out['declared_policy']['source_lease_uid'],65532)
 def test_mutation_missing_and_alias_refused(self):
  for mode in ('mutation','missing','alias','fifo'):
   with self.subTest(mode=mode),tempfile.TemporaryDirectory() as t:
    root=Path(t);m=self.fixture(root);pin=self.write(root,m);target=root/'image_layers.py';target.unlink()
    if mode=='mutation':target.write_bytes(b'changed')
    elif mode=='alias':target.symlink_to(root/'artifact_operation.py')
    elif mode=='fifo':os.mkfifo(target)
    with self.assertRaises((ValueError,OSError)):p.inspect(t,pin)
 def test_manifest_schema_digest_and_escape_refused(self):
  for mode in ('digest','file_escape','missing_file','cleanup','owner','root_alias'):
   with self.subTest(mode=mode),tempfile.TemporaryDirectory() as t:
    root=Path(t);m=self.fixture(root)
    if mode=='file_escape':m['sha256']['../outside.py']='a'*64
    elif mode=='missing_file':m['sha256'].pop('image_layers.py')
    elif mode=='cleanup':m['failed_cleanup_default']=True
    elif mode=='owner':m['rehearsal_policy']['source_owner_uid']=True
    pin=self.write(root,m)
    if mode=='digest':pin='0'*64
    path=t
    if mode=='root_alias':(root/'alias').symlink_to(root);path=str(root/'alias')
    with self.assertRaises((ValueError,OSError)):p.inspect(path,pin)
if __name__=='__main__':unittest.main()
