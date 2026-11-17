import hashlib,json,tempfile,unittest
from pathlib import Path
from release_custody import validate_state
from artifact_retention import Refuse
class CustodyTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
  self.tool=self.root/'rclone';self.tool.write_bytes(b'external tool')
  self.manifest=self.root/'release.json';self.put(self.manifest,{'version':1,'phase':'built','source':'a'*40,'image_digest':'sha256:'+'b'*64,'roots':[str(self.root)],'dependencies':[{'path':str(self.tool),'role':'executable','sha256':hashlib.sha256(self.tool.read_bytes()).hexdigest()}]})
  pin={'path':str(self.manifest),'sha256':hashlib.sha256(self.manifest.read_bytes()).hexdigest()}
  self.s={'version':1,'node_id':'d'*32,'producer':'firecontroller-guarded-install-v1','previous_generation':'e'*32,'generation':'c'*32,'lease_identity':[1,2],'current':pin,'rollback':pin,'pending':None};self.path=self.root/'state.json';self.put(self.path,self.s);self.observed={k:self.s[k] for k in ['generation','current','rollback','pending','node_id','producer','previous_generation']}
 def put(self,p,d):p.write_text(json.dumps(d));p.chmod(0o600)
 def runcheck(self):return validate_state(self.path,(1,2),self.observed)
 def test_valid_is_not_gc_authority(self):
  out=self.runcheck();self.assertFalse(out['cleanup_authorized']);self.assertIn(str(self.tool),out['protected_paths'])
 def test_missing_external_tool(self):
  self.tool.unlink()
  with self.assertRaises(Refuse):self.runcheck()
 def test_changed_manifest(self):
  self.manifest.write_text('{}')
  with self.assertRaises(Refuse):self.runcheck()
 def test_live_pointer_disagrees(self):
  self.observed['pending']=self.s['current']
  with self.assertRaises(Refuse):self.runcheck()
 def test_replaced_lease(self):
  with self.assertRaises(Refuse):validate_state(self.path,(1,3),self.observed)
 def test_pending_is_protected(self):
  self.s['pending']=self.s['current'];self.observed['pending']=self.s['current'];self.put(self.path,self.s);self.assertFalse(self.runcheck()['cleanup_authorized'])
 def test_preimage_pending_retains_existing_artifacts(self):
  release=json.loads(self.manifest.read_text());release['phase']='prepared';release['image_digest']=None
  pending=self.root/'pending.json';self.put(pending,release);pin={'path':str(pending),'sha256':hashlib.sha256(pending.read_bytes()).hexdigest()}
  self.s['pending']=pin;self.observed['pending']=pin;self.put(self.path,self.s)
  self.assertIn(str(self.tool),self.runcheck()['protected_paths'])
 def test_preimage_current_refused(self):
  release=json.loads(self.manifest.read_text());release['phase']='prepared';release['image_digest']=None;self.put(self.manifest,release)
  self.s['current']['sha256']=hashlib.sha256(self.manifest.read_bytes()).hexdigest();self.put(self.path,self.s)
  with self.assertRaises(Refuse):self.runcheck()
 def test_tampered_executable_refused(self):
  self.tool.write_bytes(b'tampered tool')
  with self.assertRaises(Refuse):self.runcheck()
 def test_missing_indirect_dependency_refused(self):
  config=self.root/'provider.conf';self.put(config,{'rclone_binary':str(self.root/'missing')})
  release=json.loads(self.manifest.read_text());release['dependencies'].append({'path':str(config),'role':'config','sha256':hashlib.sha256(config.read_bytes()).hexdigest()});self.put(self.manifest,release)
  self.s['current']['sha256']=hashlib.sha256(self.manifest.read_bytes()).hexdigest();self.put(self.path,self.s)
  with self.assertRaises(Refuse):self.runcheck()
class PreparedPublisherTests(CustodyTests):
 def setUp(self):
  super().setUp()
  import copy
  self.root.chmod(0o700);self.lock=self.root/'lease';self.lock.touch(mode=0o600);st=self.lock.stat();self.s['lease_identity']=[st.st_dev,st.st_ino];self.put(self.path,self.s)
  self.proposed=copy.deepcopy(self.s);self.proposed['generation']='f'*32;self.proposed['previous_generation']=self.s['generation']
  release=json.loads(self.manifest.read_text());release['phase']='prepared';release['image_digest']=None;p=self.root/'pending.json';self.put(p,release)
  self.proposed['pending']={'path':str(p),'sha256':hashlib.sha256(p.read_bytes()).hexdigest()}
  self.expected=hashlib.sha256(self.path.read_bytes()).hexdigest()
 def publish(self,observe=None):
  from release_custody import publish_prepared
  return publish_prepared(self.path,self.lock,self.expected,self.proposed,observe or (lambda:self.observed))
 def runcheck(self):
  st=self.lock.stat();return validate_state(self.path,(st.st_dev,st.st_ino),self.observed)
 def test_prepared_cas_publishes_no_cleanup_authority(self):
  self.assertFalse(self.publish()['cleanup_authorized']);self.assertEqual(json.loads(self.path.read_text()),self.proposed)
 def test_stale_cas_preserves_pointer(self):
  original=self.path.read_bytes();self.expected='0'*64
  with self.assertRaises(Refuse):self.publish()
  self.assertEqual(original,self.path.read_bytes())
 def test_live_change_preserves_pointer(self):
  original=self.path.read_bytes();calls=[0]
  def observe():
   calls[0]+=1;return self.observed if calls[0]==1 else {}
  with self.assertRaises(Refuse):self.publish(observe)
  self.assertEqual(original,self.path.read_bytes())
 def test_active_change_refused(self):
  self.proposed['current']=self.proposed['pending']
  with self.assertRaises(Refuse):self.publish()
 def test_pending_clear_refused(self):
  self.proposed['pending']=None
  with self.assertRaises(Refuse):self.publish()
 def test_held_lease_refuses_without_publication(self):
  import fcntl
  original=self.path.read_bytes()
  with self.lock.open('r+') as lock:
   fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
   with self.assertRaises(BlockingIOError):self.publish()
  self.assertEqual(original,self.path.read_bytes())
 def test_unresolved_pending_is_not_replaced(self):
  self.s['pending']=self.proposed['pending'];self.observed['pending']=self.s['pending'];self.put(self.path,self.s);self.expected=hashlib.sha256(self.path.read_bytes()).hexdigest()
  with self.assertRaisesRegex(Refuse,'unresolved pending'):self.publish()
if __name__=='__main__':unittest.main()
