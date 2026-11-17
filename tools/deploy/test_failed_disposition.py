"""Failure evidence only: no historical enrollment or deletion authority."""
import json
import subprocess
from unittest.mock import patch
from test_rehearsal_operation import RehearsalOperationTests
import rehearsal_operation as operation
from artifact_retention import Refuse


class FailureEvidenceTests(RehearsalOperationTests):
    def test_known_nonzero_retains_output_source_and_bound_failure(self):
        self.set_runner("raise SystemExit(23)")
        with self.assertRaises(operation.KnownForegroundFailure) as caught:
            self.run_operation()
        work = self.root / 'first'
        evidence = json.loads((work / 'failure.json').read_text())
        self.assertEqual(evidence, caught.exception.evidence)
        self.assertEqual(evidence['exit_code'], 23)
        self.assertEqual(evidence['output_sha256'], self.hash(self.source))
        self.assertEqual(evidence['lease_identity'], [self.lease.stat().st_dev, self.lease.stat().st_ino])
        self.assertEqual((work / 'data/firecontroller.sqlite').read_bytes(), self.original)
        self.assertEqual(self.source.read_bytes(), self.original)
        self.assertFalse((work / 'terminal.json').exists())
        self.assertFalse((work / 'disposal.json').exists())

    def test_timeout_has_no_known_failure_receipt(self):
        self.set_runner("import time; time.sleep(10)")
        with self.assertRaisesRegex(Refuse, 'deadline'):
            self.run_operation(timeout=.05)
        self.assertFalse((self.root / 'first/failure.json').exists())
        self.assertTrue((self.root / 'first/data/firecontroller.sqlite').exists())

    def test_replaced_lease_has_no_known_failure_receipt(self):
        self.set_runner("from pathlib import Path; p=Path(" + repr(str(self.lease)) + "); p.unlink(); p.touch(mode=0o600); raise SystemExit(1)")
        with self.assertRaisesRegex(Refuse, 'lease identity changed'):
            self.run_operation()
        self.assertFalse((self.root / 'first/failure.json').exists())

    def test_supervisor_nonzero_is_not_known_foreground_failure(self):
        with patch.object(operation.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1)):
            with self.assertRaises(Refuse) as caught:
                self.run_operation(supervised=True)
        self.assertNotIsInstance(caught.exception, operation.KnownForegroundFailure)
        self.assertFalse((self.root / 'first/failure.json').exists())
        self.assertTrue((self.root / 'first/data/firecontroller.sqlite').exists())

    def test_observed_supervised_unknown_never_becomes_foreground_disposable(self):
        import supervised_failure
        with patch.object(supervised_failure,'run_observed',return_value=(7,None)):
            with self.assertRaisesRegex(Refuse,'unknown supervised failure'):
                self.run_operation(supervised=True,observe_supervised_failure=True)
        work=self.root/'first'
        self.assertEqual((work/'data/firecontroller.sqlite').read_bytes(),self.original)
        self.assertTrue((work/'migration-runner').exists())
        self.assertFalse((work/'failure.json').exists())
        self.assertFalse((work/'supervised-failure.json').exists())
        self.assertFalse((work/'failed-equivalent-disposal.json').exists())

    def test_observed_flag_is_explicit_and_supervised_only(self):
        for value in ('yes',1,True):
            with self.assertRaisesRegex(Refuse,'observation mode'):
                self.run_operation(observe_supervised_failure=value)
        self.assertFalse((self.root/'first').exists())
