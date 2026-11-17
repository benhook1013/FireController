import ast
from pathlib import Path
import unittest
from render_split_coordinator import render
from split_test_fixture import TEMPLATE, OLD_ROOT
import hashlib

class RenderTest(unittest.TestCase):
    def test_exact_existing_functions_and_fresh_entry(self):
        raw=TEMPLATE
        fresh='/root/firecontroller-operations/release-prepared-proof-only'
        result=render(raw,fresh,hashlib.sha256(raw).hexdigest(),OLD_ROOT);ast.parse(result)
        original=raw.decode().split("if __name__=='__main__':",1)[0]
        self.assertEqual(result.split("if __name__=='__main__':",1)[0],original.replace(OLD_ROOT,fresh))
        self.assertNotIn(OLD_ROOT,result)
        self.assertIn('run(globals(),sys.argv[1])',result)
        self.assertLess(result.index("review=json.loads"),result.index('from coordinator_split_adapter import run'))
        self.assertLess(result.index('hashlib.sha256(target.read_bytes())'),result.index('from coordinator_split_adapter import run'))
        for invalid in [OLD_ROOT,fresh+'/../old','/tmp/operation']:
            with self.assertRaises(ValueError):render(raw,invalid,hashlib.sha256(raw).hexdigest(),OLD_ROOT)
        with self.assertRaises(ValueError):render(raw+b'\n',fresh,hashlib.sha256(raw).hexdigest(),OLD_ROOT)

if __name__=='__main__':unittest.main()
