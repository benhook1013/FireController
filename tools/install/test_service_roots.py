import copy
import unittest
from service_roots import inspect_roots, Invalid


def observation(args):
    return {'type':'a(sasbttttuii)', 'data':[[args[0],args,False,0,0,0,0,0,0,0]]}


class ServiceRootsTest(unittest.TestCase):
    def setUp(self):
        self.batch=observation(['/release/new/fcd','backup-push-batch','--config','/private/batch.json'])
        self.health=observation(['/usr/bin/python3','/release/script/publish.py','--fcd','/release/reader/fcd','--sha256','a'*64])

    def test_distinct_reader_is_not_replaced_by_latest(self):
        result=inspect_roots(self.batch,self.health)
        self.assertEqual(result['health_helper'],'/release/reader/fcd')
        self.assertNotEqual(result['health_helper'],result['batch_executable'])
        self.assertFalse(result['installation_ready'])
        self.assertFalse(result['closure_verified'])

    def test_shell_wrapper_and_extra_flags_refused(self):
        for args in [['/bin/sh','-c','SECRET'],['/release/new/fcd','backup-push-batch','--config','/private/batch.json','--extra']]:
            with self.assertRaises(Invalid) as error:inspect_roots(observation(args),self.health)
            self.assertNotIn('SECRET',str(error.exception))

    def test_alternate_argv_and_ignore_errors_refused(self):
        for field,value in [(0,'/other/fcd'),(2,True)]:
            b=copy.deepcopy(self.batch);b['data'][0][field]=value
            with self.assertRaises(Invalid):inspect_roots(b,self.health)

    def test_multiple_commands_refused(self):
        b=copy.deepcopy(self.batch);b['data']*=2
        with self.assertRaises(Invalid):inspect_roots(b,self.health)

    def test_bad_pin_or_path_refused(self):
        for field,value in [(3,'relative/fcd'),(3,'/a/../fcd'),(5,'b'*63),(1,'/path\nSECRET')]:
            h=copy.deepcopy(self.health);h['data'][0][1][field]=value
            with self.assertRaises(Invalid) as error:inspect_roots(self.batch,h)
            self.assertNotIn('SECRET',str(error.exception))

    def test_observation_unchanged(self):
        before=copy.deepcopy((self.batch,self.health));inspect_roots(self.batch,self.health)
        self.assertEqual(before,(self.batch,self.health))

    def test_exact_qualified_isolated_publisher_shape(self):
        args=['/usr/bin/python3','-I','-S','-B','/release/operator/publish_backup_health.py',
              '--fcd','/release/operator/bin/health-reader','--sha256','a'*64]
        result=inspect_roots(self.batch,observation(args))
        self.assertEqual(result['health_interpreter_flags'],['-I','-S','-B'])
        self.assertEqual(result['health_script'],args[4])
        self.assertEqual(result['health_helper'],args[6])
        self.assertEqual(result['health_helper_sha256'],args[8])
        self.assertNotEqual(result['health_helper'],result['batch_executable'])
        self.assertFalse(result['closure_verified'])
        self.assertFalse(result['installation_ready'])
        for flags in (['-I'],['-I','-S'],['-S','-I','-B'],['-I','-S','-B','-c']):
            with self.subTest(flags=flags),self.assertRaises(Invalid):
                inspect_roots(self.batch,observation([args[0],*flags,*args[4:]]))

    def test_original_unisolated_shape_is_not_relabelled(self):
        self.assertEqual(inspect_roots(self.batch,self.health)['health_interpreter_flags'],[])
