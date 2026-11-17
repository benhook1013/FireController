import json
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch
from test_bounded_rehearsal import BoundedTests
import bounded_rehearsal as bounded
import rehearsal_operation as operation
import failed_equivalent_repair as repair
from artifact_retention import Refuse


class InterruptedFailureTests(BoundedTests):
    def run_crash(self, boundary):
        self.set_runner('raise SystemExit(7)')
        args={k:str(v) if isinstance(v,Path) else v for k,v in self.args.items()}
        args.update(budget_lease=str(self.budget),source_lease=str(self.source_lock),discard_failed_equivalent=True)
        record=self.base/'args.json';record.write_text(json.dumps(args))
        result=subprocess.run([sys.executable,__file__,'crash',str(record),boundary],capture_output=True,timeout=10)
        self.assertEqual(result.returncode,73,result.stderr)

    def settle(self):
        return repair.settle_absent(self.args,budget_lease=self.budget,source_lease=self.source_lock)

    def test_crash_after_unlink_settles_without_deleting_anything(self):
        self.run_crash('unlinked')
        work=self.root/'first'
        self.assertFalse((work/'data/firecontroller.sqlite').exists())
        self.assertFalse((work/'failed-equivalent-disposal.json').exists())
        before={p:p.read_bytes() for p in work.iterdir() if p.is_file()}
        result=self.settle();self.assertTrue(result['failure_preserved'])
        self.assertEqual({p:p.read_bytes() for p in before},before)
        self.assertEqual(self.source.read_bytes(),self.original)
        bounded.admit(self.root)

    def test_crash_before_unlink_retains_output_and_fences(self):
        self.run_crash('intent')
        output=self.root/'first/data/firecontroller.sqlite'
        with self.assertRaisesRegex(Refuse,'never removes'):self.settle()
        self.assertEqual(output.read_bytes(),self.original)
        self.assertFalse((self.root/'first/failed-equivalent-disposal.json').exists())

    def test_boolean_intent_version_refused(self):
        self.run_crash('unlinked')
        path=self.root/'first/failed-disposal-intent.json'
        body=json.loads(path.read_text());body['version']=True;path.write_text(json.dumps(body))
        with self.assertRaisesRegex(Refuse,'unqualified interrupted'):self.settle()
        self.assertFalse((self.root/'first/failed-equivalent-disposal.json').exists())

    def test_repair_changed_source_refuses(self):
        self.run_crash('unlinked');self.source.write_bytes(b'changed')
        with self.assertRaisesRegex(Refuse,'identity mismatch'):self.settle()
        self.assertFalse((self.root/'first/failed-equivalent-disposal.json').exists())

if __name__=='__main__':
    if len(sys.argv)>1 and sys.argv[1]=='crash':
        args=json.loads(Path(sys.argv[2]).read_text());boundary=sys.argv[3]
        original_json=operation.new_json;original_unlink=Path.unlink
        def record(path,body):
            original_json(path,body)
            if boundary=='intent' and path.name=='failed-disposal-intent.json':os._exit(73)
        def unlink(path,*a,**kw):
            original_unlink(path,*a,**kw)
            if boundary=='unlinked' and path.name=='firecontroller.sqlite':os._exit(73)
        with patch.object(operation,'new_json',record),patch.object(Path,'unlink',unlink):bounded.execute(**args)
    else:unittest.main()
