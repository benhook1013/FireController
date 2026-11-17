import copy
import hashlib
import json
import unittest
from prepared_image_content import verify_content

class ContentTest(unittest.TestCase):
    def setUp(self):
        self.blobs={}
        def add(raw):
            digest='sha256:'+hashlib.sha256(raw).hexdigest();self.blobs[digest]=raw
            return {'digest':digest,'size':len(raw)}
        self.layer=add(b'exact prepared compressed layer')
        cfg=add(json.dumps({'config':{'Labels':{'org.opencontainers.image.revision':'source'}}}).encode())
        mf=add(json.dumps({'schemaVersion':2,'config':cfg,'layers':[self.layer]}).encode())
        self.report={'source':'source','binary_sha256':'binary','previous_image':'old','archive_sha256':'archive','unchanged_runtime_config':True,'source_revision_label_only':True,'added_layer_files':['fcd'],'image':'localhost/firecontroller@'+mf['digest']}
    def verify(self):return verify_content(self.report,'source','binary','old','archive',self.blobs.__getitem__)
    def test_exact_graph(self):self.assertEqual(self.verify(),self.report['image'])
    def test_same_report_missing_or_changed_layer_refuses(self):
        original=dict(self.blobs)
        for mode in ['missing','changed']:
            self.blobs=dict(original)
            if mode=='missing':del self.blobs[self.layer['digest']]
            else:self.blobs[self.layer['digest']]=b'changed'
            with self.assertRaises((KeyError,ValueError)):self.verify()
    def test_changed_source_archive_and_preimage_refuse(self):
        original=copy.deepcopy(self.report)
        for key in ['source','archive_sha256','binary_sha256','previous_image']:
            self.report=dict(original);self.report[key]='changed'
            with self.assertRaises(ValueError):self.verify()

if __name__=='__main__':unittest.main()
