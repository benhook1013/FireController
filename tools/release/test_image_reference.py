import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("image_reference", Path(__file__).with_name("image-reference.py"))
reference = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reference)


class PublishedReferenceTests(unittest.TestCase):
    def test_local_first_digest_never_replaces_published_repository(self):
        digest = "sha256:" + "a" * 64
        target = "ghcr.io/example/controller"
        self.assertEqual(reference.select_reference(target, ["controller@" + digest, target + "@" + digest]), target + "@" + digest)

    def test_missing_or_ambiguous_repository_digest_is_refused(self):
        target = "ghcr.io/example/controller"
        for values in [None, [], ["controller@sha256:" + "a" * 64], [target + "@sha256:short"], [target + "@sha256:" + letter * 64 for letter in "ab"]]:
            with self.subTest(values=values), self.assertRaises(ValueError):
                reference.select_reference(target, values)


if __name__ == "__main__":
    unittest.main()
