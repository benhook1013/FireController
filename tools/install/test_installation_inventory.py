import copy
import unittest
from installation_inventory import join_inventory, Invalid
from test_service_context import snapshot

class InventoryTest(unittest.TestCase):
    def setUp(self):
        self.b=snapshot(['/release/batch/fcd','backup-push-batch','--config','/private/batch.json'])
        self.h=snapshot(['/usr/bin/python3','/release/publisher/health.py','--fcd','/release/old-reader/fcd','--sha256','a'*64])
        self.i={'installation_ready':False,'dependencies':[
            {'role':r,'provider':p,'path':v,'state':'present'} for r,p,v in [
                ('batch_configuration',-1,'/private/batch.json'),('batch_state',-1,'/state/batch'),('source',-1,'/source'),
                ('provider_configuration',0,'/private/provider.json'),('provider_state',0,'/state/provider'),
                ('transport_executable',0,'/tools/rclone'),('transport_configuration',0,'/private/rclone.conf')]]}

    def test_distinct_roles_and_unresolved_obligations_preserved(self):
        result=join_inventory(self.b,self.h,self.i)
        self.assertEqual(result['services']['health_helper'],'/release/old-reader/fcd')
        self.assertEqual(result['services']['batch_executable'],'/release/batch/fcd')
        self.assertFalse(result['installation_ready']);self.assertFalse(result['closure_verified'])
        self.assertGreaterEqual(len(result['unresolved']),8)

    def test_missing_transport_preserved_as_missing_not_ready(self):
        self.i['dependencies'][-2]['state']='missing'
        result=join_inventory(self.b,self.h,self.i)
        self.assertEqual(result['configuration_dependencies'][-2]['state'],'missing')
        self.assertFalse(result['installation_ready'])

    def test_wrong_config_duplicate_missing_and_sparse_provider_refused(self):
        for mode in ['mismatch','duplicate','missing','sparse','ready']:
            i=copy.deepcopy(self.i)
            if mode=='mismatch':i['dependencies'][0]['path']='/other/batch.json'
            if mode=='duplicate':i['dependencies'].append(i['dependencies'][0])
            if mode=='missing':i['dependencies'].pop()
            if mode=='sparse':
                for d in i['dependencies']:
                    if d['provider']==0:d['provider']=1
            if mode=='ready':i['installation_ready']=True
            with self.subTest(mode=mode), self.assertRaises(Invalid):join_inventory(self.b,self.h,i)

    def test_copies_output_and_does_not_echo_unknown_private_fields(self):
        result=join_inventory(self.b,self.h,self.i)
        result['configuration_dependencies'][0]['path']='/changed'
        self.assertEqual(self.i['dependencies'][0]['path'],'/private/batch.json')
        self.i['dependencies'][0]['secret']='SECRET'
        with self.assertRaises(Invalid) as error:join_inventory(self.b,self.h,self.i)
        self.assertNotIn('SECRET',str(error.exception))
