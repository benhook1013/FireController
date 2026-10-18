# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Canonical copied-helper audit/projection proof over owned synthetic inputs."""
import copy
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
                controller, _ = self.fixture(store, records)
                with self.subTest(direction=direction), self.assertRaises(FrameRefusal):
                    self.acquire(controller, Routes())
            controller, evidence = self.fixture(store, records)
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
            with self.assertRaises(controller_module.ControllerError):
                self.acquire(controller, records)
            controller, evidence = self.fixture(store, records)
            evidence.review_stop_audit = lambda *a, **kw: {'complete': False}
            with self.assertRaises(controller_module.ControllerError):
                self.acquire(controller, records)
        self.assert_conserved()


if __name__ == '__main__':
    unittest.main()
