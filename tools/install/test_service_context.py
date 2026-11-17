import copy
import unittest
from service_context import EMPTY_PROPERTIES, inspect_service_graph
from service_roots import Invalid
from test_service_roots import observation


def snapshot(args):
    return {'ExecStart': observation(args), **{k:{'type':t,'data':copy.deepcopy(v)} for k,(t,v) in EMPTY_PROPERTIES.items()}}


class ServiceContextTest(unittest.TestCase):
    def setUp(self):
        self.batch=snapshot(['/release/batch/fcd','backup-push-batch','--config','/private/batch.json'])
        self.health=snapshot(['/usr/bin/python3','/release/publisher/publish.py','--fcd','/release/reader/fcd','--sha256','a'*64])

    def test_actual_observed_empty_property_shapes(self):
        result=inspect_service_graph(self.batch,self.health)
        self.assertFalse(result['installation_ready'])
        self.assertTrue(result['unresolved'])
        self.assertEqual(result['health_helper'],'/release/reader/fcd')

    def test_missing_context_is_unknown_not_empty(self):
        for name in EMPTY_PROPERTIES:
            with self.subTest(name=name):
                b=copy.deepcopy(self.batch);del b[name]
                with self.assertRaises(Invalid):inspect_service_graph(b,self.health)

    def test_environment_and_namespace_not_silently_ignored(self):
        for name,value in [('Environment',['TOKEN=SECRET']),('EnvironmentFiles',[['/private/secret',False]]),('RootDirectory','/other-root'),('BindPaths',[['/source','/target',False,0]])]:
            b=copy.deepcopy(self.batch);b[name]['data']=value
            with self.assertRaises(Invalid) as err:inspect_service_graph(b,self.health)
            self.assertNotIn('SECRET',str(err.exception))
            self.assertNotIn('/private',str(err.exception))

    def test_substituted_config_not_treated_as_literal(self):
        for value in ['/private/$CONFIG','/private/%i']:
            b=copy.deepcopy(self.batch);b['ExecStart']['data'][0][1][3]=value
            with self.assertRaises(Invalid):inspect_service_graph(b,self.health)

    def test_typed_empty_not_text_placeholder_and_credentials_not_echoed(self):
        for name in ('LoadCredential', 'LoadCredentialEncrypted', 'SetCredential', 'SetCredentialEncrypted', 'ExecStop', 'ExecStopPost'):
            for value in ('[unprintable]', [['SECRET', 'PRIVATE']]):
                b=copy.deepcopy(self.batch);b[name]['data']=value
                with self.assertRaises(Invalid) as error:inspect_service_graph(b,self.health)
                self.assertNotIn('SECRET',str(error.exception))
                self.assertNotIn('PRIVATE',str(error.exception))

    def test_no_input_mutation(self):
        before=copy.deepcopy((self.batch,self.health));inspect_service_graph(self.batch,self.health)
        self.assertEqual(before,(self.batch,self.health))
