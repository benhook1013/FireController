# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Owned synthetic assembled acquisition proof; all provider facts are fixtures."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import types
import unittest
from unittest.mock import patch

import acquire_source_history as acquire
from source_acquisition_conservation import ConservationRefusal
import test_source_acquisition_frame as frame_fixture
from test_source_acquisition_bindings import HELPERS


class AcquisitionTests(unittest.TestCase):
    def setUp(self):
        self.owned=tempfile.TemporaryDirectory()
        self.addCleanup(self.owned.cleanup)
        self.root=Path(self.owned.name)
        self.common=self.root/'handoff';self.common.mkdir(mode=0o700)
        logical=self.common/'firemud/pr-review-stack.json';logical.mkdir(parents=True,mode=0o700)
        self.database=logical.with_suffix('.sqlite3')
        HELPERS['sqlite_store'].SqliteStateStore(self.database).update(
            lambda _: HELPERS['state'].ReviewState(ordered_prs=(7,)))
        HELPERS['sqlite_review_records'].SqliteReviewRecords(self.database).bootstrap()
        self.database.chmod(0o600)
        self.capture=self.common/'firemud/capture-body';self.capture.write_bytes(b'owned original capture')
        self.gitroot=self.root/'proof';self.gitroot.mkdir(mode=0o700)
        subprocess.run(['git','-C',str(self.gitroot),'init'],check=True,capture_output=True)
        subprocess.run(['git','-C',str(self.gitroot),'remote','add','origin','https://github.com/owner/project.git'],check=True,capture_output=True)
        destination=self.root/'prepared';destination.mkdir(mode=0o700)
        plan=self.root/'plan.json'
        plan.write_text(json.dumps(dict(version=1,repository='owner/project',repository_id=17,
            default_base_ref='main',paused=True,scratch_git_writes=True,
            prs=[dict(pull_number=7,channels=['cli','hosted'])])))
        plan.chmod(0o600)
        self.args=types.SimpleNamespace(source_root=os.environ['FIRECONTROLLER_SOURCE_ROOT'],
            common=str(self.common),snapshot=str(self.database),git_root=str(self.gitroot),
            plan=str(plan),output=str(destination/'frame.json'),provenance=str(destination/'proof.json'))
        self.before=self.database.read_bytes()

    def construct(self, helpers, plan, store, records, gitroot):
        return frame_fixture.FrameTests().fixture(store,records)[0]

    def run_acquire(self, construct=None):
        github=__import__('pr_review.github',fromlist=['run_gh_query'])
        with patch.object(acquire,'construct',side_effect=construct or self.construct), \
                patch.object(github,'run_gh_query',return_value={'data':{'repository':{
                    'databaseId':17,'nameWithOwner':'owner/project','defaultBranchRef':{'name':'main'}}}}), \
                patch.object(socket,'socket',side_effect=AssertionError('no network')):
            return acquire.acquire(self.args)

    def test_assembled_frame_provenance_and_exact_retry(self):
        result=self.run_acquire()
        self.assertEqual((result['prs'],result['channels'],result['outcome']),(1,2,'created'))
        frame=Path(self.args.output).read_bytes();proof=Path(self.args.provenance).read_bytes()
        self.assertEqual(self.run_acquire()['outcome'],'unchanged')
        self.assertEqual(Path(self.args.output).read_bytes(),frame)
        self.assertEqual(Path(self.args.provenance).read_bytes(),proof)
        self.assertEqual(self.database.read_bytes(),self.before)
        self.assertEqual(self.capture.read_bytes(),b'owned original capture')
        self.assertFalse(json.loads(proof)['durable_writer_fence_established'])
        self.assertEqual(json.loads(proof)['binding']['frame_sha256'],acquire.transfer.digest(frame))

    def test_capture_changed_during_audit_cannot_publish(self):
        def changed(*args):
            self.capture.write_bytes(b'changed owned capture')
            return self.construct(*args)
        with self.assertRaises(ConservationRefusal):
            self.run_acquire(changed)
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())
        self.assertEqual(self.database.read_bytes(),self.before)

    def test_shared_or_alternate_git_root_refuses_before_provider(self):
        (self.gitroot/'.git/objects/info/alternates').write_text('/unapproved/objects\n')
        with self.assertRaises(acquire.AcquisitionRefusal):
            self.run_acquire()
        self.assertFalse(Path(self.args.output).exists())

    def test_repository_numeric_identity_mismatch_refuses(self):
        github=__import__('pr_review.github',fromlist=['run_gh_query'])
        with patch.object(github,'run_gh_query',return_value={'data':{'repository':{
                'databaseId':18,'nameWithOwner':'owner/project','defaultBranchRef':{'name':'main'}}}}):
            with self.assertRaisesRegex(acquire.AcquisitionRefusal,'source_repository_mismatch'):
                acquire.acquire(self.args)
        self.assertFalse(Path(self.args.output).exists())

    def test_git_object_symlink_cannot_escape_owned_proof_root(self):
        objects=self.gitroot/'.git/objects'
        outside=self.root/'outside-objects'
        objects.rename(outside)
        objects.symlink_to(outside,target_is_directory=True)
        with self.assertRaisesRegex(acquire.AcquisitionRefusal,'scratch_git_not_isolated'):
            self.run_acquire()
        self.assertFalse(Path(self.args.output).exists())

    def test_git_mutable_metadata_hardlink_cannot_write_outside_clone(self):
        outside=self.root/'outside-fetch-head';outside.write_text('owned original\n')
        os.link(outside,self.gitroot/'.git/FETCH_HEAD')
        with self.assertRaisesRegex(acquire.AcquisitionRefusal,'scratch_git_not_isolated'):
            self.run_acquire()
        self.assertEqual(outside.read_text(),'owned original\n')
        self.assertFalse(Path(self.args.output).exists())

    def test_interrupted_publication_retains_original_frame_without_provider_retry(self):
        publish=acquire.transfer.publish
        def interrupted(target,body):
            if target==self.args.output:
                raise OSError('owned injected failure before frame publication')
            return publish(target,body)
        with patch.object(acquire.transfer,'publish',side_effect=interrupted):
            with self.assertRaises(OSError):
                self.run_acquire()
        self.assertTrue(Path(self.args.provenance).exists())
        self.assertFalse(Path(self.args.output).exists())
        with patch.object(acquire,'construct',side_effect=AssertionError('must replay retained frame')), \
                patch.object(acquire,'repository_matches',side_effect=AssertionError('must not requery provider')):
            result=acquire.acquire(self.args)
        self.assertTrue(result['replayed'])
        proof=json.loads(Path(self.args.provenance).read_bytes())
        self.assertEqual(json.loads(Path(self.args.output).read_bytes()),proof['retained_frame'])


if __name__=='__main__':
    unittest.main()
