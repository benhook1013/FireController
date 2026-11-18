import hashlib
import unittest
from browser_asset_binding import Refuse, asset_paths, verify_live

class BrowserBindingTests(unittest.TestCase):
    def test_qualified_new_assets_not_previous_literals(self):
        html=b'<script src="/assets/new.js"></script><link rel="stylesheet" href="/assets/new.css">'
        data={"/assets/new.js":b"new js", "/assets/new.css":b"new css"}
        binding={"source":"a"*40,"assets":{p:hashlib.sha256(b).hexdigest() for p,b in data.items()}}
        self.assertEqual(verify_live(binding,"a"*40,html,data.__getitem__),sorted(data))
        with self.assertRaises(Refuse):verify_live(binding,"b"*40,html,data.__getitem__)
        with self.assertRaises(Refuse):verify_live(binding,"a"*40,html.replace(b"new.js",b"old.js"),data.__getitem__)
        with self.assertRaises(Refuse):verify_live(binding,"a"*40,html,lambda p:b"wrong")
    def test_unsafe_missing_duplicate_refused(self):
        for html in [b'',b'<script src="https://other/a.js"></script>',b'<script src="/assets/../a.js"></script>',b'<script src="/assets/a.js"></script><script src="/assets/a.js"></script>']:
            with self.assertRaises(Refuse):asset_paths(html)

if __name__=="__main__":unittest.main()
