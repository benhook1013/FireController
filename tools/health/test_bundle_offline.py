"""Exercise actual bundle bytes with the qualified reader, offline and root-only."""
import hashlib
import importlib.util
import json
import pathlib
import tarfile
import unittest

ROOT=pathlib.Path(__file__).parent

def load(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module

qualification=load('offline_fixture',ROOT/'test_offline_qualification.py')
packager=load('health_packager',ROOT/'package_bundle.py')

class PackagedOfflineQualification(qualification.OfflineQualification):
    def prepare_publisher(self,root,original):
        bundle=root/'health.tar.gz'
        expected=packager.build(original,bundle)
        unpacked=root/'unpacked';unpacked.mkdir(mode=0o700)
        # This fixture extracts only the three exact expected regular members;
        # it is not a general-purpose production installer.
        with tarfile.open(bundle,'r:gz') as archive:
            members=archive.getmembers()
            self.assertEqual([m.name for m in members],['manifest.json','publish_backup_health.py','bin/health-reader'])
            bodies={}
            for member in members:
                self.assertTrue(member.isfile());self.assertLessEqual(member.size,128<<20)
                bodies[member.name]=archive.extractfile(member).read(member.size+1)
                self.assertEqual(len(bodies[member.name]),member.size)
        manifest=json.loads(bodies['manifest.json'])
        self.assertEqual(manifest,expected);self.assertFalse(manifest['installation_ready'])
        self.assertEqual(manifest['launch'][-1],qualification.PIN)
        for name,record in manifest['files'].items():
            body=bodies[name]
            self.assertEqual(len(body),record['bytes'])
            self.assertEqual(hashlib.sha256(body).hexdigest(),record['sha256'])
            destination=unpacked/name;destination.parent.mkdir(mode=0o700,exist_ok=True)
            destination.write_bytes(body);destination.chmod(0o755 if name=='bin/health-reader' else 0o644)
        return load('packaged_publisher',unpacked/'publish_backup_health.py'),unpacked/'bin/health-reader'

if __name__=='__main__':unittest.main()