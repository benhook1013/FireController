# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""CLI refusal privacy and useful recovery without source or provider access."""
from contextlib import redirect_stderr, redirect_stdout
import io
import json
import unittest
from unittest.mock import patch

import acquire_source_history as acquire
from source_acquisition_frame import FrameRefusal


class AcquisitionErrorTests(unittest.TestCase):
    def invoke(self, **behavior):
        args = [part for name in ('source-root', 'common', 'snapshot', 'git-root',
                                 'plan', 'output', 'provenance')
                for part in ('--' + name, '/unused/' + name)]
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(acquire, 'acquire', **behavior), \
                redirect_stdout(stdout), redirect_stderr(stderr):
            code = acquire.main(args)
        return code, stdout.getvalue(), stderr.getvalue()

    def test_known_busy_source_refusal_has_action_without_private_details(self):
        code, stdout, stderr = self.invoke(
            side_effect=FrameRefusal('source_requests_not_clear'))
        self.assertEqual((code, stdout), (3, ''))
        self.assertEqual(json.loads(stderr), {
            'error': 'source_acquisition_refused',
            'reason': 'source_requests_not_clear',
            'next_action': 'Resolve retained source requests and verify they are clear before a fresh acquisition.',
        })
        self.assertEqual(len(stderr.splitlines()), 1)
        self.assertLess(len(stderr.encode()), 256)

    def test_other_known_guard_gives_its_specific_recovery(self):
        code, stdout, stderr = self.invoke(
            side_effect=FrameRefusal('source_state_changed'))
        self.assertEqual((code, stdout), (3, ''))
        self.assertEqual(json.loads(stderr)['reason'], 'source_state_changed')
        self.assertIn('fresh sealed handoff', json.loads(stderr)['next_action'])

    def test_unknown_or_provider_errors_never_echo_exception_content(self):
        private = '/private/source secret-token provider-body'
        for error in (FrameRefusal(private), RuntimeError(private),
                      acquire.AcquisitionRefusal(private),
                      RuntimeError('source_requests_not_clear'),
                      FrameRefusal('source_requests_not_clear', private)):
            with self.subTest(kind=type(error).__name__):
                code, stdout, stderr = self.invoke(side_effect=error)
                self.assertEqual((code, stdout), (3, ''))
                self.assertEqual(stderr, '{"error":"source_acquisition_refused"}\n')

    def test_exception_arguments_are_not_formatted_or_coerced(self):
        class PrivateValue:
            def __str__(self):
                raise AssertionError('must not format private value')

            def __repr__(self):
                raise AssertionError('must not format private value')

        for error in (FrameRefusal(PrivateValue()), RuntimeError(PrivateValue())):
            code, stdout, stderr = self.invoke(side_effect=error)
            self.assertEqual((code, stdout), (3, ''))
            self.assertEqual(stderr, '{"error":"source_acquisition_refused"}\n')

    def test_success_does_not_add_refusal_or_diagnostic_fields(self):
        result = dict(kind='source_owner_frame', outcome='created', prs=1,
                      channels=1, sha256='a' * 64, replayed=False)
        code, stdout, stderr = self.invoke(return_value=result)
        self.assertEqual((code, stderr), (0, ''))
        self.assertEqual(json.loads(stdout), result)


if __name__ == '__main__':
    unittest.main()
