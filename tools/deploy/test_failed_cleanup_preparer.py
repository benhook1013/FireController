import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import prepare_rehearsal as prepare
from artifact_retention import Refuse

class PreparerCleanupTests(unittest.TestCase):
    def fixture(self,root):
        policy={name:('/synthetic/'+name) for name in prepare.FIELDS}
        policy['source_lease_uid']=policy['source_owner_uid']=0
        config={name:'value' for name in prepare.INPUTS};config['mode']='bounded'
        hashes={}
        for name in prepare.FAILED_HELPERS:
            body=(Path(prepare.__file__).parent/name).read_bytes();(root/name).write_bytes(body);hashes[name]=hashlib.sha256(body).hexdigest()
        (root/'manifest.json').write_text(json.dumps({'sha256':hashes}))
        return config,policy,hashes
    def test_default_has_no_observation_or_cleanup_opt_in(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);c,p,_=self.fixture(root);text=prepare.render(c,p,root)
            self.assertNotIn('observe_supervised_failure',text);self.assertNotIn('discard_failed_equivalent',text)
    def test_explicit_true_with_complete_hashed_package_enables_both(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);c,p,_=self.fixture(root);c['failed_equivalent_cleanup']=True
            text=prepare.render(c,p,root);compile(text,'rehearse.py','exec')
            self.assertIn("'observe_supervised_failure': True",text);self.assertIn("'discard_failed_equivalent': True",text)
    def test_missing_or_changed_dependency_refuses(self):
        for change in ('missing','changed'):
            with self.subTest(change=change),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);c,p,h=self.fixture(root);c['failed_equivalent_cleanup']=True
                if change=='missing':del h['supervised_equivalent.py'];(root/'manifest.json').write_text(json.dumps({'sha256':h}))
                else:(root/'supervised_equivalent.py').write_text('# changed')
                with self.assertRaises(Refuse):prepare.render(c,p,root)
    def test_truthy_nonboolean_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);c,p,_=self.fixture(root);c['failed_equivalent_cleanup']=1
            with self.assertRaises(Refuse):prepare.render(c,p,root)
