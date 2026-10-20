# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Canonical copied-helper audit/projection proof over owned synthetic inputs."""
import copy
import dataclasses
import importlib
import importlib.util
from pathlib import Path
import unittest

from source_acquisition_frame import FrameRefusal, acquire_frame
from source_acquisition_bindings import bind_source
from test_source_acquisition_bindings import HELPERS
import test_source_acquisition_bindings as binding_fixture

controller_module = importlib.import_module('pr_review.controller')
acceptance = importlib.import_module('pr_review.acceptance')
policy = importlib.import_module('pr_review.policy')
spec = importlib.util.spec_from_file_location('export_transfer_validator',
    Path(__file__).with_name('prepare_source_controls.py'))
transfer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(transfer)


class FrameTests(unittest.TestCase):
    setUp = binding_fixture.WiringTests.setUp
    assert_conserved = binding_fixture.WiringTests.assert_conserved

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
        self.assert_conserved()

    def test_empty_window_transfer_does_not_manufacture_history_or_stop(self):
        with bind_source(HELPERS, common=self.common, snapshot=self.database) as (store, records):
            controller, evidence = self.fixture(store, records)
            evidence.rows = {'cli': [], 'hosted': []}
            frame = self.acquire(controller, records)
            self.assertEqual(frame['prs'][0]['channels'], {'cli': [], 'hosted': []})
            self.assertGreater(evidence.audits, 0)
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
