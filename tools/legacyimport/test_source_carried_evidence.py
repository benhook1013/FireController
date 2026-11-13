# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Owned carried-audit proof; optional explicitly selected source implementation."""
from contextlib import ExitStack
from copy import deepcopy
import hashlib
import importlib
import os
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import source_carried_evidence as carried
from source_carried_frame import canonical, row_digest
import test_source_obligation_results as results


class RecordingEvidence:
    """Record the audit handoff, leaving the real result verifier in place."""

    def __init__(self, repo, records, histories):
        self.repo, self.records, self.histories = repo, records, histories
        self.base_calls = []
        self.audit_result = dict(complete=True, active_reservations=['still active'],
                                 ambiguous_responses=['still ambiguous'],
                                 unknown_review_evidence=['still unknown'])
        self.during_audit = None

    @staticmethod
    def _uncheckpointed_hosted_observation(pr, state):
        return dict(pr=pr, head=state.head_sha,
                    checkpoint=f'trigger-uncheckpointed:{state.response_id}', held=True,
                    reason='completed Hosted review has no valid public checkpoint and requires adjudication')

    def history(self, pr, channel, *, now=None):
        return self.histories[(pr, channel)]

    def _audit_complete_hosted_history(self, pr, fingerprints, anchor, *, payload,
                                      allow_historical_unmatched=False, now=None):
        self.base_calls.append(dict(pr=pr, fingerprints=fingerprints, anchor=anchor,
            payload=payload, allow_historical_unmatched=allow_historical_unmatched, now=now))
        if self.during_audit:
            self.during_audit(payload)
        return self.audit_result


def no_external_io(stack):
    for name in ('builtins.open', 'os.open', 'subprocess.run', 'subprocess.Popen', 'socket.socket'):
        stack.enter_context(patch(name, side_effect=AssertionError('no ambient I/O')))


class CarriedEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.owner = results.ResultTests('test_exact_proof_originals_and_replay_preserve_inputs_without_credit')
        self.owner.setUp()
        self.owner.records.history = Mock(return_value=self.owner.history)
        self.helpers = dict(self.owner.modules,
            runtime=SimpleNamespace(LiveEvidence=RecordingEvidence),
            state=SimpleNamespace(observation_fingerprint=lambda row: hashlib.sha256(canonical(row)).hexdigest()))
        observation = RecordingEvidence._uncheckpointed_hosted_observation(7,
            SimpleNamespace(head_sha=results.HEAD, response_id=201))
        self.histories = {(7, 'hosted'): [observation], (7, 'cli'): []}
        self.anchor = dict(child_head=results.HEAD)
        self.build()

    def build(self, selected=None):
        cls = carried.evidence_class(self.helpers, {7: (results.RUN,)} if selected is None else selected)
        self.reader = cls(results.REPO, self.owner.records, self.histories)
        return cls

    def audit(self, pr=7, fingerprints=('incoming-fingerprint',)):
        with ExitStack() as stack:
            no_external_io(stack)
            return self.reader._audit_complete_hosted_history(pr, fingerprints, self.anchor,
                payload=self.owner.payload, allow_historical_unmatched=True, now='owned-cutoff')

    def test_exact_positive_proof_adds_fingerprint_without_changing_rows_payload_or_audit(self):
        before = deepcopy((self.owner.payload, self.owner.history, self.owner.artifacts, self.histories))
        original_class = self.helpers['runtime'].LiveEvidence
        original_method = original_class._audit_complete_hosted_history
        self.assertIs(self.audit(), self.reader.audit_result)
        call = self.reader.base_calls[0]
        observation = self.histories[(7, 'hosted')][0]
        fingerprint = self.helpers['state'].observation_fingerprint(observation)
        self.assertEqual(call['fingerprints'], ('incoming-fingerprint', fingerprint))
        self.assertIs(call['payload'], self.owner.payload)
        self.assertIs(call['anchor'], self.anchor)
        self.assertTrue(call['allow_historical_unmatched'])
        self.assertEqual(call['now'], 'owned-cutoff')
        proof = self.reader.carried_result_proofs[(7, results.RUN)]
        self.assertEqual(proof['observation_sha256'], row_digest(observation))
        self.assertEqual(proof['observation_fingerprint'], fingerprint)
        self.assertEqual(proof['audited_payload_sha256'], row_digest(self.owner.payload))
        self.assertEqual(proof['result']['deferred'], [(7, 'hosted')])
        self.assertEqual([row['kind'] for row in proof['result']['obligations']], ['result', 'finding', 'finding'])
        self.assertNotIn('counts', proof['result'])
        self.assertEqual(before, (self.owner.payload, self.owner.history, self.owner.artifacts, self.histories))
        self.owner.records.history.assert_called_once_with(7, include_legacy_routes=True, include_display=False)
        self.assertIs(self.helpers['runtime'].LiveEvidence, original_class)
        self.assertIs(original_class._audit_complete_hosted_history, original_method)

    def test_selection_is_copied_and_unknown_pr_preserves_incoming_fingerprints(self):
        selected = {7: (results.RUN,)}
        self.build(selected)
        selected.clear()
        self.audit()
        self.assertEqual(len(self.reader.carried_result_proofs), 1)
        self.build()
        self.owner.records.history.reset_mock()
        expected = ('first', 'second', 'first')
        result = self.audit(pr=8, fingerprints=expected)
        self.assertIs(result, self.reader.audit_result)
        self.assertEqual(self.reader.base_calls[0]['fingerprints'], expected)
        self.owner.records.history.assert_not_called()
        self.assertEqual(self.reader.carried_result_proofs, {})

    def test_unbound_changed_or_nonterminal_positive_proof_refuses_before_base(self):
        for mutation in ('missing-run', 'wrong-pr', 'changed-review', 'changed-ledger', 'active-run'):
            with self.subTest(mutation=mutation):
                self.setUp()
                if mutation == 'missing-run':
                    self.owner.history['runs'].clear()
                elif mutation == 'wrong-pr':
                    self.owner.history['runs'][0]['source_pr'] = 8
                elif mutation == 'changed-review':
                    self.owner.payload['data']['repository']['pullRequest']['reviews']['nodes'][0]['databaseId'] = 202
                elif mutation == 'changed-ledger':
                    self.owner.history['findings'][0]['detail'] += ' changed'
                else:
                    self.owner.history['runs'][0]['outcome'] = 'active'
                with self.assertRaises(results.ResultRefusal):
                    self.audit()
                self.assertEqual(self.reader.base_calls, [])
                self.assertEqual(self.reader.carried_result_proofs, {})

    def test_missing_duplicate_or_changed_canonical_held_observation_refuses_before_base(self):
        for mutation in ('absent', 'duplicate', 'changed-head', 'completed', 'active', 'unheld'):
            with self.subTest(mutation=mutation):
                self.setUp()
                rows = self.histories[(7, 'hosted')]
                if mutation == 'absent':
                    rows.clear()
                elif mutation == 'duplicate':
                    rows.append(deepcopy(rows[0]))
                else:
                    rows[0].update({'changed-head': dict(head='b' * 40), 'completed': dict(completed=True),
                                    'active': dict(active_review=True), 'unheld': dict(held=False)}[mutation])
                with self.assertRaisesRegex(carried.CarriedEvidenceRefusal, '^carried_evidence_unverified$'):
                    self.audit()
                self.assertEqual(self.reader.base_calls, [])
                self.assertEqual(self.reader.carried_result_proofs, {})

    def test_two_positive_runs_cannot_claim_same_observation_identity(self):
        second = 'other-owned-run'
        for name, key in (('runs', 'run_id'), ('attempts', 'attempt_id')):
            row = deepcopy(self.owner.history[name][0])
            row.update(run_id=second)
            row[key] = second
            self.owner.history[name].append(row)
        self.owner.history['findings'].extend(dict(row, run_id=second)
                                             for row in list(self.owner.history['findings']))

        def artifacts(run):
            value = deepcopy(self.owner.artifacts)
            import json
            metadata = json.loads(value['metadata'])
            metadata['trigger_record']['sqlite_attempt_id'] = run
            value['metadata'] = json.dumps(metadata)
            return value

        self.owner.records.attempt_artifacts.side_effect = artifacts
        self.build({7: (results.RUN, second)})
        with self.assertRaises(carried.CarriedEvidenceRefusal):
            self.audit()
        self.assertEqual(self.owner.records.attempt_artifacts.call_count, 2)
        self.assertEqual(self.reader.base_calls, [])
        self.assertEqual(self.reader.carried_result_proofs, {})

    def test_payload_mutation_or_base_failure_never_publishes_result_proof(self):
        for mutation in ('payload', 'exception'):
            with self.subTest(mutation=mutation):
                self.setUp()

                def during(payload):
                    if mutation == 'payload':
                        payload['data']['repository']['pullRequest']['headRefOid'] = 'b' * 40
                    else:
                        raise ValueError('owned base audit failure')

                self.reader.during_audit = during
                error = carried.CarriedEvidenceRefusal if mutation == 'payload' else ValueError
                with self.assertRaises(error):
                    self.audit()
                self.assertEqual(len(self.reader.base_calls), 1)
                self.assertEqual(self.reader.carried_result_proofs, {})

    def test_invalid_explicit_selections_refuse_without_source_reads(self):
        for selected in (None, {}, [], {True: (results.RUN,)}, {0: (results.RUN,)},
                         {2**31: (results.RUN,)}, {7: [results.RUN]}, {7: ()},
                         {7: (results.RUN, results.RUN)}, {7: ('bad\nrun',)},
                         {7: ('x' * 101,)}, {7: ('é' * 51,)}):
            with self.subTest(selected=selected):
                with self.assertRaises(carried.CarriedEvidenceRefusal):
                    carried.evidence_class(self.helpers, selected)
        self.owner.records.history.assert_not_called()


@unittest.skipUnless(os.environ.get('FIRECONTROLLER_SOURCE_ROOT'), 'explicit trusted source root not selected')
class TrustedCarriedEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.owner = results.TrustedResultTests('test_exact_proof_originals_and_replay_preserve_inputs_without_credit')
        self.owner.setUp()
        self.helpers = dict(self.owner.modules,
            runtime=importlib.import_module('pr_review.runtime'), state=importlib.import_module('pr_review.state'))
        source = Path(os.environ['FIRECONTROLLER_SOURCE_ROOT']) / 'dev-tools'
        for name in ('runtime', 'state'):
            self.assertTrue(Path(self.helpers[name].__file__).resolve().is_relative_to(source.resolve()))
        self.base = self.helpers['runtime'].LiveEvidence
        self.owner.records.history = Mock(return_value=self.owner.history)
        self.owner.payload['data']['repository']['pullRequest'].update(baseRefName='main', baseRefOid='b' * 40)
        self.anchor = dict(child_head=results.HEAD, live_base_ref='main', live_base_tip='b' * 40)
        self.live = SimpleNamespace(pull_request=Mock(return_value=SimpleNamespace(number=7,
            head_sha=results.HEAD, base_ref_name='main', base_sha='b' * 40)))
        self.rows = [self.base._uncheckpointed_hosted_observation(7,
            SimpleNamespace(head_sha=results.HEAD, response_id=201))]
        self.rows.extend(dict(pr=7, checkpoint='review-threads:' + str(n), held=True, finding_only_hold=True)
                         for n in (301, 302))

    def reader(self, carried_class):
        cls = carried.evidence_class(self.helpers, {7: (results.RUN,)}) if carried_class else self.base
        reader = cls(results.REPO, self.live, None, self.owner.records)
        # Supply exact cached canonical observations at the collection boundary.
        reader._histories = {(7, 'hosted'): self.rows, (7, 'cli'): []}
        return reader

    def audit(self, reader):
        with ExitStack() as stack:
            stack.enter_context(patch.object(self.base, '_complete_trigger_paths', return_value=['owned-trigger']))
            stack.enter_context(patch.object(self.helpers['hosted'], 'load_trigger_record',
                return_value=deepcopy(self.owner.metadata['trigger_record'])))
            no_external_io(stack)
            return reader._audit_complete_hosted_history(7, (), self.anchor, payload=self.owner.payload)

    def test_real_source_audit_recognizes_only_missing_checkpoint_and_old_class_is_unchanged(self):
        original_method = self.base._audit_complete_hosted_history
        before = deepcopy((self.owner.payload, self.owner.history, self.owner.artifacts, self.rows))
        baseline = self.audit(self.reader(False))
        reader = self.reader(True)
        actual = self.audit(reader)
        expected = deepcopy(baseline)
        self.assertEqual(expected['unmatched_responses'], ['completed Hosted response has no checkpoint or prior audit'])
        expected['unmatched_responses'].clear()
        self.assertEqual(actual, expected)
        self.assertEqual(actual['finding_only_findings'], ['review-threads:301', 'review-threads:302'])
        self.assertEqual(before, (self.owner.payload, self.owner.history, self.owner.artifacts, self.rows))
        self.assertEqual(len(reader.carried_result_proofs), 1)
        proof = reader.carried_result_proofs[(7, results.RUN)]
        self.assertEqual(proof['observation_fingerprint'], self.helpers['state'].observation_fingerprint(self.rows[0]))
        self.assertEqual(proof['result']['deferred'], [(7, 'hosted')])
        self.assertEqual(self.audit(self.reader(False)), baseline)
        self.assertIs(self.helpers['runtime'].LiveEvidence, self.base)
        self.assertIs(self.base._audit_complete_hosted_history, original_method)

    def test_real_source_keeps_other_active_and_unknown_audit_classifications(self):
        self.owner.payload['data']['repository']['pullRequest']['comments']['nodes'].append(
            dict(databaseId=999, author={'login': 'owner'}, body='@coderabbitai full review',
                 createdAt='2026-01-02T03:06:05Z', updatedAt='2026-01-02T03:06:05Z'))
        self.rows.append(dict(pr=7, checkpoint='review-threads:777', held=True, unstable=True))
        baseline = self.audit(self.reader(False))
        actual = self.audit(self.reader(True))
        self.assertTrue(actual['active_reservations'])
        self.assertEqual(actual['unknown_review_evidence'], ['review-threads:777'])
        expected = deepcopy(baseline)
        expected['unmatched_responses'].remove('completed Hosted response has no checkpoint or prior audit')
        self.assertEqual(actual, expected)

    def test_real_source_moved_anchor_refuses_and_does_not_publish_positive_proofs(self):
        self.anchor['live_base_tip'] = 'c' * 40
        reader = self.reader(True)
        with self.assertRaises(self.helpers['runtime'].ControllerError):
            self.audit(reader)
        self.assertEqual(reader.carried_result_proofs, {})


if __name__ == '__main__':
    unittest.main()
