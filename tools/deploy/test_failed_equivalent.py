import json
from unittest.mock import patch
from test_bounded_rehearsal import BoundedTests
import bounded_rehearsal as bounded
import rehearsal_operation as operation
from artifact_retention import Refuse


class FailedEquivalentTests(BoundedTests):
    def test_failed_identical_opt_in_preserves_failure_and_source_frees_slot(self):
        self.set_runner('raise SystemExit(7)')
        for name in ('first','second'):
            with self.assertRaises(operation.KnownForegroundFailure):
                self.bounded(operation=name, discard_failed_equivalent=True)
            work=self.root/name
            self.assertFalse((work/'data/firecontroller.sqlite').exists())
            self.assertTrue((work/'failure.json').exists())
            self.assertTrue((work/'failed-disposal-intent.json').exists())
            receipt=json.loads((work/'failed-equivalent-disposal.json').read_text())
            self.assertEqual(receipt['bytes'],len(self.original))
            self.assertEqual(receipt['evidence']['exit_code'],7)
            self.assertEqual(self.source.read_bytes(),self.original)
            self.assertFalse((work/'terminal.json').exists())

    def test_failed_unique_retained_even_opt_in(self):
        self.set_runner(self.code+'\nraise SystemExit(7)')
        with self.assertRaises(operation.KnownForegroundFailure):
            self.bounded(discard_failed_equivalent=True)
        self.assertTrue((self.root/'first/data/firecontroller.sqlite').exists())
        self.assertFalse((self.root/'first/failed-disposal-intent.json').exists())
        with self.assertRaisesRegex(Refuse,'explicit disposition'):
            self.bounded(operation='second',discard_failed_equivalent=True)

    def test_failed_identical_default_retained(self):
        self.set_runner('raise SystemExit(7)')
        with self.assertRaises(operation.KnownForegroundFailure):self.bounded()
        self.assertTrue((self.root/'first/data/firecontroller.sqlite').exists())
        self.assertFalse((self.root/'first/failed-equivalent-disposal.json').exists())

    def test_replaced_budget_lease_refuses_before_intent(self):
        self.set_runner('raise SystemExit(7)')
        original=bounded.operation.execute
        def run(**args):
            try:return original(**args)
            finally:self.budget.unlink();self.budget.touch(mode=0o600)
        with patch.object(bounded.operation,'execute',side_effect=run):
            with self.assertRaisesRegex(Refuse,'lease replaced'):
                self.bounded(discard_failed_equivalent=True)
        self.assertTrue((self.root/'first/data/firecontroller.sqlite').exists())
        self.assertFalse((self.root/'first/failed-disposal-intent.json').exists())

    def test_source_change_after_failure_refuses_before_intent(self):
        self.set_runner('raise SystemExit(7)')
        original=bounded.operation.execute
        def run(**args):
            try:return original(**args)
            finally:self.source.write_bytes(b'changed')
        with patch.object(bounded.operation,'execute',side_effect=run):
            with self.assertRaisesRegex(Refuse,'identity mismatch'):
                self.bounded(discard_failed_equivalent=True)
        self.assertEqual((self.root/'first/data/firecontroller.sqlite').read_bytes(),self.original)
        self.assertFalse((self.root/'first/failed-disposal-intent.json').exists())

    def test_empty_start_after_real_failure_refuses_before_delete(self):
        self.set_runner('raise SystemExit(7)')
        original=bounded.operation.execute
        def run(**args):
            try:return original(**args)
            finally:(self.root/'first/started.json').write_text('{}')
        with patch.object(bounded.operation,'execute',side_effect=run):
            with self.assertRaisesRegex(Refuse,'complete failure'):
                self.bounded(discard_failed_equivalent=True)
        self.assertEqual((self.root/'first/data/firecontroller.sqlite').read_bytes(),self.original)
        self.assertFalse((self.root/'first/failed-disposal-intent.json').exists())

    def test_missing_both_digests_cannot_free_admission(self):
        self.set_runner('raise SystemExit(7)')
        with self.assertRaises(operation.KnownForegroundFailure):self.bounded(discard_failed_equivalent=True)
        work=self.root/'first'
        for name in ('failure.json','failed-disposal-intent.json','failed-equivalent-disposal.json'):
            p=work/name;body=json.loads(p.read_text());proof=body if name=='failure.json' else body['evidence']
            del proof['source_sha256'];del proof['output_sha256'];p.write_text(json.dumps(body))
        before={p.name:p.read_bytes() for p in work.iterdir() if p.is_file()}
        with self.assertRaisesRegex(Refuse,'complete failure'):
            self.bounded(operation='second',discard_failed_equivalent=True)
        self.assertFalse((self.root/'second').exists())
        self.assertEqual({p.name:p.read_bytes() for p in work.iterdir() if p.is_file()},before)
