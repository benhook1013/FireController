import hashlib,json,os,tempfile,unittest,sys
from pathlib import Path
from artifact_operation import execute
from artifact_retention import Refuse
from deployment_duplicates import validate_producer_receipts

class ProducerReceipts(unittest.TestCase):
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
  self.root=Path(self.temp.name);self.root.chmod(0o700)
  self.lock=self.root/'lease';self.lock.touch(mode=0o600)
  self.op=self.root/'operation';self.op.mkdir(mode=0o700)
  self.manifest=self.op/'enrollment.json';self.manifest.write_text(json.dumps({'workflow_receipt':'terminal.json'}));self.manifest.chmod(0o600)
  execute(self.op,self.lock,[sys.executable,'-c','pass'],self.manifest)
  self.marker=self.op/'terminal.json';s=self.lock.stat();self.identity=(s.st_dev,s.st_ino)
  self.entry={'path':str(self.op),'producer_manifest':str(self.manifest),'marker':str(self.marker),'marker_sha256':hashlib.sha256(self.marker.read_bytes()).hexdigest()}
 def validate(self):validate_producer_receipts({'terminal_roots':[self.entry]},self.identity)
 def test_real_producer_receipt(self):self.validate()
 def test_replaced_lease_refused(self):
  self.identity=(self.identity[0],self.identity[1]+1)
  with self.assertRaises(Refuse):self.validate()
 def test_changed_enrollment_refused(self):
  self.manifest.write_text(json.dumps({'workflow_receipt':'terminal.json','changed':True}))
  with self.assertRaises(Refuse):self.validate()
 def test_arbitrary_marker_refused(self):
  self.marker.write_text('{}');self.entry['marker_sha256']=hashlib.sha256(self.marker.read_bytes()).hexdigest()
  with self.assertRaises(Refuse):self.validate()
 def test_unenrolled_result_refused(self):
  self.marker.write_text(json.dumps({'state':'producer_finished_unenrolled'}));self.entry['marker_sha256']=hashlib.sha256(self.marker.read_bytes()).hexdigest()
  with self.assertRaises(Refuse):self.validate()
 def test_wrong_root_refused(self):
  d=json.loads(self.marker.read_text());d['root']=str(self.root);self.marker.write_text(json.dumps(d));self.entry['marker_sha256']=hashlib.sha256(self.marker.read_bytes()).hexdigest()
  with self.assertRaises(Refuse):self.validate()
 def test_apply_entrypoint_is_hard_disabled(self):
  from deployment_duplicates import execute as collect_command
  results=self.root/'results';results.mkdir(mode=0o700)
  config=self.root/'config.json';config.write_text(json.dumps({'apply':True}));config.chmod(0o600)
  with self.assertRaisesRegex(Refuse,'automatic disposal disabled'):
   collect_command(config,self.lock,results)
if __name__=='__main__':unittest.main()
