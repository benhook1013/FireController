# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Synthetic batch and optional copied-helper proof over owned inputs."""
import copy
import contextlib
import dataclasses
from enum import Enum
import importlib
import importlib.util
import io
import os
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, call, patch

from source_acquisition_frame import FrameRefusal, acquire_frame
from source_acquisition_bindings import bind_source
if os.environ.get('FIRECONTROLLER_SOURCE_ROOT'):
    from test_source_acquisition_bindings import HELPERS
    import test_source_acquisition_bindings as binding_fixture
    controller_module = importlib.import_module('pr_review.controller')
    acceptance = importlib.import_module('pr_review.acceptance')
    policy = importlib.import_module('pr_review.policy')
else:
    HELPERS = None
spec = importlib.util.spec_from_file_location('export_transfer_validator',
    Path(__file__).with_name('prepare_source_controls.py'))
transfer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(transfer)


class BatchQuiescenceTests(unittest.TestCase):
    """Stdlib synthetic boundaries; no executable source package is required."""
    def setUp(self):
        class Channel(str, Enum):
            CLI = 'cli'
            HOSTED = 'hosted'
        self.policy = SimpleNamespace(Channel=Channel)
        self.state = SimpleNamespace(ordered_prs=(7, 8), to_dict=lambda: {'ordered_prs': [7, 8]})
        self.evidence = SimpleNamespace(repository_cli_lock_status=Mock(return_value='clear'),
            request_history=Mock(return_value=[]))
        self.github = SimpleNamespace()
        self.controller = SimpleNamespace(isolated_fixture=False,
            store=SimpleNamespace(load=Mock(return_value=self.state)), github=self.github,
            _evidence_provider=self.evidence,
            _reconciliation=Mock(return_value=({7: SimpleNamespace(head='a'*40)}, object())),
            _reconciled_anchor=Mock(return_value=object()), _check_stop_evidence=Mock(),
            _policy_history=Mock(return_value=[]),
            _project_cli_hosted_reservations=lambda history, *_: history,
            _project_cli_streak_lineage=lambda history, *_: history,
            _actual_hosted_overlap_anchor=lambda *_: None)
        self.records = SimpleNamespace(list_routes=Mock(return_value=[]))
        self.validator = Mock()

    def identities(self, head='a'):
        return {pr: dict(number=pr, headRefOid=head*40,
                         comments={'nodes': []}, reviews={'nodes': []}) for pr in (7, 8)}

    def requests(self):
        return {(pr, channel): [] for pr in (7, 8) for channel in ('cli', 'hosted')}

    def batch(self):
        self.github.batch_pull_requests = Mock(side_effect=lambda _: self.identities())
        self.evidence.request_history_batch = Mock(side_effect=lambda *_: self.requests())
        self.evidence.request_history.side_effect = AssertionError('batch path must not read per PR')

    def acquire(self):
        return acquire_frame(self.controller, self.records, self.policy,
            repository_id=17, snapshot_sha256='d'*64, selected={7: ('cli',)},
            writer_pause_confirmed=True, history_validator=self.validator)

    def refused(self):
        with self.assertRaises(FrameRefusal) as failure:
            self.acquire()
        self.assertEqual(failure.exception.args, ('source_requests_not_clear',))
        self.validator.assert_not_called()

    def test_fresh_before_and_after_batches_cover_full_stack(self):
        self.batch()
        first, second = self.identities('a'), self.identities('a')
        second[7]['commits'] = {'nodes': [{'statusCheckRollup': {'state': 'SUCCESS'}}]}
        self.github.batch_pull_requests.side_effect = [first, second]
        frame = self.acquire()
        self.assertTrue(frame['quiescent'])
        self.assertEqual(self.github.batch_pull_requests.call_args_list, [call((7, 8))]*2)
        self.assertEqual(self.evidence.request_history_batch.call_args_list,
                         [call((7, 8), first), call((7, 8), second)])
        self.assertIs(self.controller._reconciliation.call_args.kwargs['live_identities'], first)
        self.assertEqual(self.controller._reconciliation.call_args.kwargs['refresh_prs'], set())
        self.assertEqual(frame['prs'], [{'pull_number': 7, 'channels': {'cli': []}}])
        self.evidence.request_history.assert_not_called()
        self.assertEqual(self.state.to_dict(), {'ordered_prs': [7, 8]})

    def test_final_observed_head_base_or_review_activity_movement_refuses_frame(self):
        for pr in (7, 8):
            for key, value in (('headRefOid', 'b'*40), ('baseRefOid', 'c'*40),
                               ('baseRefName', 'changed-base'),
                               ('comments', {'nodes': [{'databaseId': 3, 'body': 'new checkpoint'}]}),
                               ('reviews', {'nodes': [{'databaseId': 4}]})):
                with self.subTest(pr=pr, key=key):
                    self.batch()
                    self.validator.reset_mock()
                    first, second = self.identities(), self.identities()
                    second[pr][key] = value
                    self.github.batch_pull_requests.side_effect = [first, second]
                    with self.assertRaises(FrameRefusal) as failure:
                        self.acquire()
                    self.assertEqual(failure.exception.args, ('source_state_changed',))
                    self.validator.assert_not_called()

    def test_incomplete_extra_or_malformed_identity_keys_refuse_before_request_batch(self):
        cases = [None, [], {}, {7: {'number': 7}}, dict(self.identities(), extra={'number': 9}),
                 {7: None, 8: {'number': 8}}, {7: {'number': '7'}, 8: {'number': 8}},
                 {7: {'number': 8}, 8: {'number': 8}}, {'7': {'number': 7}, 8: {'number': 8}}]
        for value in cases:
            with self.subTest(value=value):
                self.batch()
                self.github.batch_pull_requests.side_effect = None
                self.github.batch_pull_requests.return_value = value
                self.refused()
                self.evidence.request_history_batch.assert_not_called()

    def test_incomplete_extra_or_malformed_request_keys_and_lists_refuse(self):
        cases = [None, [], {}, {(7, 'cli'): []},
                 dict(self.requests(), extra=[]),
                 {**self.requests(), (9, 'cli'): []},
                 {**self.requests(), (7, 'all'): []},
                 {**self.requests(), (7, 'cli', 'extra'): []}]
        for rows in (None, {}, (), '', iter(())):
            cases.append({**self.requests(), (8, 'hosted'): rows})
        for value in cases:
            with self.subTest(value=value):
                self.batch()
                self.evidence.request_history_batch.side_effect = None
                self.evidence.request_history_batch.return_value = value
                self.refused()

    def test_any_request_in_unselected_pr_or_channel_refuses_on_either_pass(self):
        for key in self.requests():
            for pass_number in (1, 2):
                with self.subTest(key=key, pass_number=pass_number):
                    self.batch()
                    occupied = {**self.requests(), key: [{'terminal': True}]}
                    self.evidence.request_history_batch.side_effect = (
                        [occupied] if pass_number == 1 else [self.requests(), occupied])
                    self.refused()
                    self.assertEqual(self.github.batch_pull_requests.call_count, pass_number)

    def test_second_pass_revalidates_identity_and_request_coverage(self):
        for mode in ('identity', 'requests'):
            with self.subTest(mode=mode):
                self.batch()
                if mode == 'identity':
                    self.github.batch_pull_requests.side_effect = [self.identities(), {7: {'number': 7}}]
                else:
                    self.evidence.request_history_batch.side_effect = [self.requests(), {(7, 'cli'): []}]
                self.refused()
                self.assertEqual(self.github.batch_pull_requests.call_count, 2)

    def test_partial_capabilities_preserve_legacy_full_stack_fallback(self):
        for identity_api, request_api in ((None, None), (Mock(), None), (None, Mock()), (False, Mock())):
            with self.subTest(identity_api=identity_api, request_api=request_api):
                self.github.batch_pull_requests = identity_api
                self.evidence.request_history_batch = request_api
                self.evidence.request_history = Mock(side_effect=lambda *_: ())
                self.assertTrue(self.acquire()['quiescent'])
                self.assertEqual(self.evidence.request_history.call_args_list,
                    [call(pr, channel) for _ in range(2)
                     for pr in (7, 8) for channel in ('cli', 'hosted')])
                for api in (identity_api, request_api):
                    if isinstance(api, Mock):
                        api.assert_not_called()
        self.validator.reset_mock()
        self.evidence.request_history = Mock(side_effect=lambda pr, channel: [{}] if (pr, channel) == (8, 'hosted') else [])
        self.refused()

    def test_lock_uncertainty_refuses_before_batch_reads(self):
        self.batch()
        self.evidence.repository_cli_lock_status.return_value = 'unknown'
        with self.assertRaises(FrameRefusal) as failure:
            self.acquire()
        self.assertEqual(failure.exception.args, ('source_cli_lock_uncertain',))
        self.github.batch_pull_requests.assert_not_called()
        self.evidence.request_history_batch.assert_not_called()

    def test_final_projection_uses_post_audit_local_histories_for_both_audit_contracts(self):
        for audit_name in ('_check_stop_evidence', '_check_transfer_evidence'):
            with self.subTest(audit=audit_name):
                self.setUp()
                before = {channel: [dict(pr=7, checkpoint='before-' + channel)]
                          for channel in ('cli', 'hosted')}
                refreshed = {channel: [dict(pr=7, checkpoint='after-' + channel, correction=True)]
                             for channel in ('cli', 'hosted')}
                current = copy.deepcopy(before)
                captured = {}

                def reconcile(state, *, history_cache, **kwargs):
                    history_cache.update({(7, channel): rows for channel, rows in before.items()})
                    history_cache[(8, 'cli')] = ['unselected cache entry']
                    captured['cache'] = history_cache
                    return {7: SimpleNamespace(head='a'*40)}, object()

                def audit(*args, **kwargs):
                    # A collector refreshes its own snapshot without modifying
                    # the outer reconciliation cache passed to this adapter.
                    current.update(copy.deepcopy(refreshed))

                def project(state, pr, channel, reconciliation, *, history_cache):
                    key = pr, channel.value
                    if key not in history_cache:
                        history_cache[key] = self.evidence.history(*key)
                    return history_cache[key]

                self.controller._reconciliation.side_effect = reconcile
                setattr(self.controller, audit_name, Mock(side_effect=audit))
                self.controller._policy_history.side_effect = project
                self.evidence.history = Mock(side_effect=lambda pr, channel: current[channel])
                self.controller._project_cli_hosted_reservations = Mock(side_effect=lambda history, *_: history)
                self.controller._project_cli_streak_lineage = Mock(side_effect=lambda history, *_: history)
                frame = acquire_frame(self.controller, self.records, self.policy,
                    repository_id=17, snapshot_sha256='d'*64, selected={7: ('cli', 'hosted')},
                    writer_pause_confirmed=True, history_validator=self.validator)
                self.assertEqual(frame['prs'][0]['channels'], refreshed)
                self.assertEqual(self.evidence.history.call_args_list, [call(7, 'cli'), call(7, 'hosted')])
                self.assertEqual(captured['cache'][(8, 'cli')], ['unselected cache entry'])
                self.controller._project_cli_hosted_reservations.assert_called_once()
                reservation_args = self.controller._project_cli_hosted_reservations.call_args.args
                self.assertEqual(reservation_args[:2], (refreshed['cli'], refreshed['hosted']))
                self.controller._project_cli_streak_lineage.assert_called_once()


@unittest.skipUnless(HELPERS, 'explicit trusted source root not selected')
class FrameTests(unittest.TestCase):
    def setUp(self):
        binding_fixture.WiringTests.setUp(self)

    def assert_conserved(self):
        binding_fixture.WiringTests.assert_conserved(self)

    def fixture(self, store, records):
        head, parent = 'a' * 40, 'b' * 40
        github = acceptance.FixtureGitHub({7: controller_module.LivePullRequest(
            7, head, 'main', parent, head_ref='feature')})
        git = acceptance.FixtureGit(dict(
            branch_heads={'main': parent, 'feature': head},
            ancestors=[[parent, head]], merge_bases={parent+'...'+head: parent},
            test_merge_trees={parent+'...'+head: 'c'*40},
            patch_ids={parent+'...'+head: 'owned-patch'}))

        class Evidence:
            def __init__(self):
                self.lock, self.requests, self.audits = 'clear', {}, 0
                self.rows = {channel: [dict(
                    pr=7, channel=channel, head=head, child_head=head,
                    parent_identity='main', parent_head=parent, merge_base=parent,
                    patch_id='owned-patch', checkpoint='checkpoint-'+channel,
                    source_run_id='run-'+channel, completed=True, attributable=True,
                    anchored=True, provisional=False, accepted=0, raw=0)]
                    for channel in ('cli', 'hosted')}

            def history(self, pr, channel):
                return copy.deepcopy(self.rows[channel])

            def repository_cli_lock_status(self):
                return self.lock

            def request_history(self, pr, channel):
                return self.requests.get((pr, channel), [])

            def review_stop_audit(self, pr, anchor, retained_ambiguous_fingerprints=()):
                self.audits += 1
                return dict(complete=True, head=head, anchor=dict(anchor),
                    channel_histories=copy.deepcopy(self.rows), active_reservations=[],
                    unmatched_responses=[], ambiguous_responses=[],
                    unresolved_findings=[], retained_ambiguous=[])

        evidence = Evidence()
        controller = controller_module.ReviewController(
            store=store, github=github, git=git, evidence=evidence,
            default_base_ref='main', repository='owner/project', isolated_fixture=False)
        return controller, evidence

    def acquire(self, controller, records, **changes):
        args=dict(repository_id=17, snapshot_sha256='d'*64,
                  selected={7: ('cli', 'hosted')}, writer_pause_confirmed=True,
                  history_validator=transfer.selected_history)
        args.update(changes)
        return acquire_frame(controller, records, policy, **args)

    def test_real_reconciliation_consumes_admitted_batch_without_serial_identity_reads(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            identity = dict(number=7, headRefOid='a'*40, baseRefOid='b'*40,
                baseRefName='main', headRefName='feature', state='OPEN',
                mergeable='MERGEABLE', changedFiles=1,
                headRepository={'nameWithOwner': 'owner/project'},
                comments={'nodes': []}, reviews={'nodes': []})
            controller.github.batch_pull_requests = Mock(side_effect=lambda _: {7: copy.deepcopy(identity)})
            controller.github.pull_request = Mock(side_effect=AssertionError('redundant serial identity read'))
            evidence.request_history_batch = Mock(return_value={(7, 'cli'): [], (7, 'hosted'): []})
            frame = self.acquire(controller, records)
            self.assertTrue(frame['projection_complete'])
            self.assertEqual(controller.github.batch_pull_requests.call_count, 2)
            controller.github.pull_request.assert_not_called()
        self.assert_conserved()

    def test_fresh_cli_grant_with_zero_reviews_preserves_one_remaining(self):
        state = HELPERS['sqlite_store'].SqliteStateStore(self.database).load()
        allocation = HELPERS['state'].ReviewAllocation(pr=7, channel='cli',
            head='a'*40, parent_identity='main', parent_head='b'*40,
            merge_base='b'*40, patch_id='owned-patch', baseline_checkpoints=(),
            reason='Synthetic new additional CLI grant',
            min_additional_completed=0, max_additional_completed=1)
        HELPERS['sqlite_store'].SqliteStateStore(self.database).update(
            lambda _: dataclasses.replace(state, allocations={'7:cli': allocation}))
        self.before = self.database.read_bytes()
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            evidence.rows = {'cli': [dict(pr=7, channel='cli', head='a'*40,
                checkpoint='scope-timeline:complete', scope_timeline=True,
                scope_timeline_complete=True, completed=False, attributable=False,
                anchored=False, provisional=False, non_counting=True, accepted=0, raw=0)], 'hosted': []}
            expected_rows = copy.deepcopy(evidence.rows)
            before_state = store.load().to_dict()
            if getattr(controller, '_check_transfer_evidence', None) is None:
                # Older helpers retain their positive stop-checkpoint contract;
                # an empty grant must not silently manufacture transfer proof.
                frame = None
                with contextlib.redirect_stdout(io.StringIO()) as output, \
                        contextlib.redirect_stderr(io.StringIO()) as errors, \
                        self.assertRaises(controller_module.ControllerError) as failure:
                    frame = self.acquire(controller, records)
                self.assertEqual(failure.exception.args,
                    ('review stop requires a completed attributable checkpoint',))
                self.assertIsNone(frame)
                self.assertEqual((output.getvalue(), errors.getvalue()), ('', ''))
            else:
                frame = self.acquire(controller, records)
                self.assertEqual(frame['prs'][0]['channels']['hosted'], [])
                self.assertEqual(len(frame['prs'][0]['channels']['cli']), 1)
                self.assertTrue(frame['prs'][0]['channels']['cli'][0]['scope_timeline_complete'])
                self.assertFalse(frame['prs'][0]['channels']['cli'][0]['completed'])
                helpers = dict(HELPERS, controller=controller_module, policy=policy)
                facts = transfer.control_facts(helpers, store.load(),
                    {'pull_number':7, 'channel':'cli'}, frame['prs'][0]['channels']['cli'])
                self.assertEqual(facts, dict(minimum_remaining=0, maximum_remaining=1,
                    initial_dry=0, taper=3, tapered=False, closed=False))
            self.assertEqual(evidence.rows, expected_rows)
            self.assertGreater(evidence.audits, 0)
            self.assertEqual(store.load().to_dict(), before_state)
        self.assert_conserved()

    def test_empty_window_transfer_does_not_manufacture_history_or_stop(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            evidence.rows = {'cli': [], 'hosted': []}
            before_state = store.load().to_dict()
            if getattr(controller, '_check_transfer_evidence', None) is None:
                frame = None
                with contextlib.redirect_stdout(io.StringIO()) as output, \
                        contextlib.redirect_stderr(io.StringIO()) as errors, \
                        self.assertRaises(controller_module.ControllerError) as failure:
                    frame = self.acquire(controller, records)
                self.assertEqual(failure.exception.args,
                    ('review stop requires a completed attributable checkpoint',))
                self.assertIsNone(frame)
                self.assertEqual((output.getvalue(), errors.getvalue()), ('', ''))
            else:
                frame = self.acquire(controller, records)
                self.assertEqual(frame['prs'][0]['channels'], {'cli': [], 'hosted': []})
            self.assertGreater(evidence.audits, 0)
            self.assertEqual(evidence.rows, {'cli': [], 'hosted': []})
            self.assertEqual(store.load().to_dict(), before_state)
            state = store.load()
            live, reconciliation = controller._reconciliation(state, evidence_prs={7}, prefetch_complete_history=True)
            current = controller._reconciled_anchor(7, live[7], reconciliation)
            with self.assertRaises(controller_module.ControllerError):
                controller._check_stop_evidence(state, 7, policy.Channel.CLI,
                    current, reconciliation, checkpoint_pin=None)
        self.assert_conserved()

    def test_empty_window_still_refuses_raw_activity_unknown_and_movement(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            cases = [
                {'active_reservations':['active']},
                {'unmatched_responses':['unattributed']},
                {'unknown_review_evidence':['unknown']},
                {'unresolved_findings':['pending'], 'finding_only_findings':['pending'], 'unknown_review_evidence':[]},
                {'request_preparation_only':True},
                {'complete':False},
            ]
            for fields in cases:
                with self.subTest(fields=fields):
                    controller, evidence = self.fixture(store, records)
                    evidence.rows = {'cli': [], 'hosted': []}
                    original = evidence.review_stop_audit
                    def changed(*args, **kwargs):
                        return dict(original(*args, **kwargs), **fields)
                    evidence.review_stop_audit = changed
                    with self.assertRaises(controller_module.ControllerError):
                        self.acquire(controller, records)
        self.assert_conserved()

    def test_empty_selected_window_still_refuses_other_channel_pending_findings_and_capture(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            for mode in ('pending_findings', 'unfinalized_capture', 'duplicate_identity'):
                with self.subTest(mode=mode):
                    controller, evidence = self.fixture(store, records)
                    evidence.rows['cli'] = []
                    if mode == 'pending_findings':
                        evidence.rows['hosted'][0].update(accepted=1, raw=1, source_resolution_status='finding_pending')
                    elif mode == 'unfinalized_capture':
                        evidence.rows['hosted'][0].update(completed=False, attributable=False,
                            checkpoint='pending-capture:fixture')
                    else:
                        evidence.rows['cli'] = [dict(pr=7, channel='cli', head='a'*40,
                            checkpoint='same', completed=False, attributable=False, anchored=False,
                            provisional=False, non_counting=True, accepted=0, raw=0)] * 2
                    with self.assertRaises(controller_module.ControllerError):
                        self.acquire(controller, records, selected={7: ('cli',)})
        self.assert_conserved()

    def test_empty_window_real_sqlite_incoming_and_outgoing_routes_refuse(self):
        for source, target in ((7, 8), (8, 7)):
            with self.subTest(source=source, target=target):
                original = HELPERS['sqlite_store'].SqliteStateStore(self.database).load()
                route = HELPERS['state'].FindingRoute(source_pr=source,
                    source_channel='cli', source_review='checkpoint-route',
                    source_finding='fixture-finding', observations=('Synthetic retained obligation',),
                    target_pr=target)
                HELPERS['sqlite_store'].SqliteStateStore(self.database).update(
                    lambda _: dataclasses.replace(original, routes=(route,)))
                self.before = self.database.read_bytes()
                with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
                    controller, evidence = self.fixture(store, records)
                    evidence.rows = {'cli': [], 'hosted': []}
                    self.assertEqual(len(records.list_routes(status='open',
                        include_legacy_routes=True)), 1)
                    with self.assertRaises(FrameRefusal) as error:
                        self.acquire(controller, records)
                    self.assertEqual(error.exception.args, ('source_routes_not_clear',))
                self.assert_conserved()

    def test_transfer_capability_fallback_and_malformed_capability(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            controller._check_transfer_evidence = None
            self.assertTrue(self.acquire(controller, records)['obligations_clear'])
            evidence.rows['cli'] = []
            with self.assertRaises(controller_module.ControllerError):
                self.acquire(controller, records)
            controller, _ = self.fixture(store, records)
            controller._check_transfer_evidence = False
            with self.assertRaises(FrameRefusal) as error:
                self.acquire(controller, records)
            self.assertEqual(error.exception.args, ('source_transfer_audit_unsupported',))
        self.assert_conserved()

    def test_canonical_audit_and_projection_success(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            frame = self.acquire(controller, records)
            self.assertTrue(frame['obligations_clear'])
            self.assertEqual(set(frame['prs'][0]['channels']), {'cli', 'hosted'})
            self.assertEqual(frame['prs'][0]['channels']['cli'][0]['checkpoint'], 'checkpoint-cli')
            self.assertGreater(evidence.audits, 0)
        self.assert_conserved()

    def test_post_audit_live_evidence_cache_reprojects_fresh_rows_without_provider_reads(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, fixture = self.fixture(store, records)
            runtime = HELPERS['runtime']
            evidence = runtime.LiveEvidence('owner/project', controller.github, store, records)
            evidence._histories = {(7, channel): copy.deepcopy(rows)
                                  for channel, rows in fixture.rows.items()}
            fresh = copy.deepcopy(fixture.rows)
            for channel, rows in fresh.items():
                rows[0].update(checkpoint='post-audit-' + channel, source_run_id='post-run-' + channel,
                    non_counting=True, streak_break_before=True, lineage_proven_to_next=True,
                    current_candidate_descendant_proven=True)
            fresh['cli'].append(dict(fresh['cli'][0], checkpoint='post-audit-cli-second',
                                     source_run_id='post-run-cli-second'))
            fresh['cli'].append(dict(pr=7, channel='cli', head='a'*40, checkpoint='retained-correction',
                correction=True, completed=False, attributable=False, anchored=False, accepted=0))
            expected_raw = copy.deepcopy(fresh)
            evidence.repository_cli_lock_status = Mock(return_value='clear')
            evidence.request_history = Mock(return_value=[])
            evidence.prefetch_history_payloads = Mock()
            evidence._payload = Mock(side_effect=AssertionError('extra payload/provider read'))

            def refreshed_audit(pr, anchor, **kwargs):
                evidence._histories = {(pr, channel): copy.deepcopy(rows)
                                       for channel, rows in fresh.items()}
                return dict(complete=True, head='a'*40, anchor=dict(anchor),
                    channel_histories=copy.deepcopy(fresh), active_reservations=[],
                    unmatched_responses=[], ambiguous_responses=[], unresolved_findings=[],
                    retained_ambiguous=[])

            evidence.review_stop_audit = Mock(side_effect=refreshed_audit)
            controller._evidence_provider = evidence

            def transfer_audit(state, pr, channel, current, reconciliation, **kwargs):
                # Exercise the real audit acceptance while retaining only the
                # provider-local refresh, as an explicit transfer adapter may.
                return controller._stop_audit(pr, current, ())

            controller._check_transfer_evidence = transfer_audit
            with patch.object(evidence, 'history', wraps=evidence.history) as history, \
                    patch('socket.socket', side_effect=AssertionError('network forbidden')), \
                    patch('subprocess.run', side_effect=AssertionError('subprocess forbidden')):
                frame = self.acquire(controller, records)
            channels = frame['prs'][0]['channels']
            self.assertEqual([row['checkpoint'] for row in channels['cli']],
                ['post-audit-cli', 'post-audit-cli-second', 'retained-correction'])
            self.assertEqual(channels['hosted'][0]['checkpoint'], 'post-audit-hosted')
            self.assertTrue(channels['cli'][-1]['correction'])
            self.assertNotIn('non_counting', channels['cli'][0])
            self.assertTrue(channels['cli'][0]['lineage_proven_to_next'])
            self.assertNotIn('streak_break_before', channels['cli'][1])
            self.assertNotIn('lineage_proven_to_next', channels['cli'][1])
            self.assertNotIn('current_candidate_descendant_proven', channels['cli'][1])
            self.assertNotIn('non_counting', channels['hosted'][0])
            self.assertEqual(evidence._histories, {(7, channel): rows for channel, rows in expected_raw.items()})
            self.assertEqual(history.call_args_list[-2:], [call(7, 'cli'), call(7, 'hosted')])
            evidence._payload.assert_not_called()
            self.assertEqual(evidence.review_stop_audit.call_count, 2)
        self.assert_conserved()

    def test_canonical_batch_apis_use_each_fresh_identity_without_external_calls(self):
        runtime = HELPERS['runtime']
        github_module = importlib.import_module('pr_review.github')
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            # Exercise both real batch methods while every provider and
            # per-request observation remains an owned synthetic boundary.
            live = object.__new__(runtime.LiveGitHub)
            live.repo = 'owner/project'
            live.pull_request = controller.github.pull_request
            controller.github = live
            batch_reader = runtime.LiveEvidence('owner/project', live, store, records)
            first = {7: dict(number=7, headRefOid='a'*40, baseRefOid='b'*40,
                baseRefName='main', headRefName='feature', state='OPEN',
                mergeable='MERGEABLE', changedFiles=1,
                headRepository={'nameWithOwner': 'owner/project'},
                comments={'nodes': []}, reviews={'nodes': []})}
            second = copy.deepcopy(first)
            second[7]['commits'] = {'nodes': [{'statusCheckRollup': {'state': 'SUCCESS'}}]}
            evidence.request_history_batch = batch_reader.request_history_batch
            evidence.request_history = Mock(side_effect=AssertionError('per-PR acquisition fallback forbidden'))
            with patch.object(github_module, 'fetch_pr_identity_batch', side_effect=[first, second]) as identities, \
                    patch.object(batch_reader, '_request_history', return_value=[]) as requests, \
                    patch.object(batch_reader, 'prefetch_history_payloads') as prefetch, \
                    patch('socket.socket', side_effect=AssertionError('network forbidden')), \
                    patch('subprocess.run', side_effect=AssertionError('subprocess forbidden')):
                frame = self.acquire(controller, records)
            self.assertTrue(frame['quiescent'])
            self.assertEqual(identities.call_args_list, [call('owner/project', (7,))]*2)
            self.assertEqual(requests.call_count, 4)
            for index, request in enumerate(requests.call_args_list):
                self.assertIs(request.kwargs['identity'], first[7] if index < 2 else second[7])
                self.assertIs(request.kwargs['defer_full'], True)
            self.assertEqual(prefetch.call_args_list, [call([]), call([])])
            evidence.request_history.assert_not_called()
            def pending(pr, channel, **_kwargs):
                return [{'active_reservation': True, 'checkpoint': 'trigger:fixture'}] if channel == 'hosted' else []
            payload = {'data': {'repository': {'pullRequest': {'number': 7, 'headRefOid': 'a'*40}}}}
            with patch.object(github_module, 'fetch_pr_identity_batch', side_effect=[first, second]), \
                    patch.object(batch_reader, '_request_history', side_effect=pending), \
                    patch.object(batch_reader, 'prefetch_history_payloads') as prefetch, \
                    patch.object(batch_reader, '_payload', return_value=payload), \
                    patch.object(batch_reader, '_current_hosted_history', return_value=[]) as terminal, \
                    patch('socket.socket', side_effect=AssertionError('network forbidden')), \
                    patch('subprocess.run', side_effect=AssertionError('subprocess forbidden')):
                self.assertTrue(self.acquire(controller, records)['quiescent'])
            self.assertEqual(prefetch.call_args_list, [call([7]), call([7])])
            self.assertEqual(terminal.call_count, 2)
            malformed = copy.deepcopy(first)
            malformed[7]['headRefOid'] = 'not-an-exact-head'
            with patch.object(github_module, 'fetch_pr_identity_batch', return_value=malformed), \
                    patch.object(batch_reader, '_request_history', side_effect=AssertionError('malformed identity reached requests')) as requests, \
                    self.assertRaises(controller_module.ControllerError):
                self.acquire(controller, records)
            requests.assert_not_called()
        self.assert_conserved()

    def test_accepted_pending_refuses_and_resolved_accepted_is_preserved(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            evidence.rows['cli'][0].update(accepted=1, raw=1, source_resolution_status='finding_pending')
            with self.assertRaises(controller_module.ControllerError):
                self.acquire(controller, records)
            controller, evidence = self.fixture(store, records)
            evidence.rows['cli'][0].update(accepted=1, raw=1, source_resolution_status='resolved')
            frame = self.acquire(controller, records)
            self.assertEqual(frame['prs'][0]['channels']['cli'][0]['accepted'], 1)
            self.assertEqual(frame['prs'][0]['channels']['cli'][0]['source_resolution_status'], 'resolved')
        self.assert_conserved()

    def test_both_route_directions_and_other_channel_activity_refuse(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            for direction in ('target_pr', 'source_pr'):
                class Routes:
                    def list_routes(self, **kwargs):
                        return [{'outside_queue': True}] if direction in kwargs else []
                controller, evidence = self.fixture(store, records)
                evidence.rows['cli'] = []
                with self.subTest(direction=direction), self.assertRaises(FrameRefusal):
                    self.acquire(controller, Routes())
            controller, evidence = self.fixture(store, records)
            evidence.rows['cli'] = []
            evidence.requests[(7, 'hosted')] = [{'active_reservation': True}]
            with self.assertRaises(FrameRefusal):
                self.acquire(controller, records, selected={7: ('cli',)})
        self.assert_conserved()

    def test_source_valid_legacy_positive_proof_cannot_masquerade_as_transferable(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            reviewed = 'e' * 40
            controller.git._ancestors.add((reviewed, 'a' * 40))
            evidence.rows['cli'][0].update(head=reviewed, child_head=reviewed,
                accepted=1, raw=1, corrected_state=True)
            # The actual canonical source audit permits its legacy descendant
            # proof; the unchanged transfer owner must still reject the handoff.
            with self.assertRaises(transfer.Refusal) as result:
                self.acquire(controller, records)
            self.assertEqual(result.exception.code, 'controls_unsettled')
            self.assertGreater(evidence.audits, 0)
        self.assert_conserved()

    def test_pause_unknown_lock_missing_checkpoint_and_incomplete_audit_refuse(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            with self.assertRaises(FrameRefusal):
                self.acquire(controller, records, writer_pause_confirmed=False)
            evidence.lock = 'unknown'
            with self.assertRaises(FrameRefusal):
                self.acquire(controller, records)
            controller, evidence = self.fixture(store, records)
            evidence.rows['cli'] = []
            state = store.load()
            cache = {}
            live, reconciliation = controller._reconciliation(state, evidence_prs={7}, history_cache=cache, prefetch_complete_history=True)
            current = controller._reconciled_anchor(7, live[7], reconciliation)
            # Transfer clearance does not grant ordinary stop authority.
            with self.assertRaises(controller_module.ControllerError):
                controller._check_stop_evidence(state, 7, policy.Channel.CLI,
                    current, reconciliation, checkpoint_pin=None, history_cache=cache)
            controller, evidence = self.fixture(store, records)
            evidence.review_stop_audit = lambda *a, **kw: {'complete': False}
            with self.assertRaises(controller_module.ControllerError):
                self.acquire(controller, records)
        self.assert_conserved()


if __name__ == '__main__':
    unittest.main()
