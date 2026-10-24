# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Owned synthetic assembled acquisition proof; all provider facts are fixtures."""
from contextlib import ExitStack, nullcontext, redirect_stderr, redirect_stdout
import io
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
import source_acquisition_conservation as conservation
from source_acquisition_conservation import ConservationRefusal
from source_acquisition_bindings import BindingRefusal
from source_acquisition_frame import FrameRefusal


class AcquisitionMainTests(unittest.TestCase):
    arguments=[value for name in ('source-root','common','snapshot','git-root','plan','output','provenance')
               for value in ('--'+name,'/explicit-fixture/'+name)]

    def refused(self, failure):
        stdout,stderr=io.StringIO(),io.StringIO()
        with patch.object(acquire,'acquire',side_effect=failure), \
                redirect_stdout(stdout),redirect_stderr(stderr):
            result=acquire.main(self.arguments)
        self.assertEqual(result,3)
        self.assertEqual(stdout.getvalue(),'')
        self.assertEqual(len(stderr.getvalue().splitlines()),1)
        return json.loads(stderr.getvalue())

    def test_database_limit_cli_default_positive_range_and_duplicate_guard(self):
        for tail, expected in (([], 32 << 20), (["--max-database-bytes", str(512 << 20)], 512 << 20),
                               (["--max-database-bytes", str((1 << 63)-1)], (1 << 63)-1)):
            with patch.object(acquire, 'acquire', return_value={}) as called, redirect_stdout(io.StringIO()):
                self.assertEqual(acquire.main(self.arguments+tail), 0)
            self.assertEqual(acquire.transfer.snapshot_limit(called.call_args.args[0]), expected)
        for tail in (["--max-database-bytes", "0"], ["--max-database-bytes", "-1"],
                     ["--max-database-bytes", str(1 << 63)], ["--max-database-bytes", "PRIVATE_BAD_VALUE"],
                     ["--max-database-bytes", "1", "--max-database-bytes", "2"]):
            with patch.object(acquire, 'acquire') as called, redirect_stderr(io.StringIO()) as errors, self.assertRaises(SystemExit) as failure:
                acquire.main(self.arguments+tail)
            self.assertEqual(failure.exception.code, 2)
            self.assertEqual(errors.getvalue(), '{"error":"arguments_invalid"}\n')
            called.assert_not_called()

    def test_capture_caps_cli_defaults_positive_range_and_safe_refusals(self):
        for tail, expected, explicit in (
                ([], dict(max_bytes=128 << 20, max_entries=20000), False),
                (['--max-capture-bytes', str(768 << 20)], dict(max_bytes=768 << 20, max_entries=20000), True),
                (['--max-capture-entries', '50000'], dict(max_bytes=128 << 20, max_entries=50000), True),
                (['--max-capture-bytes', str((1 << 63)-1), '--max-capture-entries', str((1 << 63)-2)],
                 dict(max_bytes=(1 << 63)-1, max_entries=(1 << 63)-2), True)):
            with self.subTest(tail=tail), patch.object(acquire, 'acquire', return_value={}) as called, redirect_stdout(io.StringIO()):
                self.assertEqual(acquire.main(self.arguments+tail), 0)
            self.assertEqual(acquire.capture_limits(called.call_args.args[0]), (expected, explicit))
        for option, maximum in (('--max-capture-bytes', (1 << 63)-1), ('--max-capture-entries', (1 << 63)-2)):
            for value in ('0', '-1', '1.5', 'True', 'False', 'PRIVATE_BAD_VALUE', str(maximum+1)):
                with self.subTest(option=option, value=value), patch.object(acquire, 'acquire') as called, \
                        redirect_stderr(io.StringIO()) as errors, self.assertRaises(SystemExit) as failure:
                    acquire.main(self.arguments+[option, value])
                self.assertEqual(failure.exception.code, 2)
                self.assertEqual(errors.getvalue(), '{"error":"arguments_invalid"}\n')
                called.assert_not_called()
            with patch.object(acquire, 'acquire') as called, redirect_stderr(io.StringIO()) as errors, self.assertRaises(SystemExit):
                acquire.main(self.arguments+[option, '1', option, '2'])
            self.assertEqual(errors.getvalue(), '{"error":"arguments_invalid"}\n')
            called.assert_not_called()
        for value in (True, False, 1, None):
            with self.subTest(nontext=value), self.assertRaises(acquire.argparse.ArgumentTypeError):
                acquire.positive_capture_limit(value, (1 << 63)-1)

    def test_programmatic_invalid_capture_caps_precede_all_input_reads(self):
        for name, maximum in (('max_capture_bytes', (1 << 63)-1), ('max_capture_entries', (1 << 63)-2)):
            for value in (0, -1, True, False, '1', 1.5, maximum+1):
                with self.subTest(name=name, value=value), \
                        patch.object(acquire, 'exact_path', side_effect=AssertionError('no input resolution')) as paths, \
                        patch.object(acquire.transfer, 'load_helpers', side_effect=AssertionError('no source helpers')) as helpers, \
                        self.assertRaises(acquire.AcquisitionRefusal) as failure:
                    acquire.acquire(types.SimpleNamespace(**{name: value}))
                self.assertEqual(failure.exception.args, ('capture_limits_invalid',))
                paths.assert_not_called()
                helpers.assert_not_called()

    def test_known_local_refusal_families_preserve_stable_diagnostics(self):
        for failure,expected in (
                (acquire.AcquisitionRefusal('source_repository_mismatch'),{'error':'source_repository_mismatch'}),
                (acquire.AcquisitionRefusal('source_preflight_deadline_exceeded'),
                 {'error':'source_preflight_deadline_exceeded', 'next_action':
                  'No frame was published. Check provider responsiveness and the complete-stack acquisition path before retrying.'}),
                (acquire.transfer.Refusal('controls_unsettled',private='/private/helper/body secret=fixture'),{'error':'controls_unsettled'}),
                (BindingRefusal('source_database_not_sealed'),{'error':'source_database_not_sealed'}),
                (FrameRefusal('source_requests_not_clear'),{'error':'source_requests_not_clear',
                    'next_action':'Resolve retained source requests and verify they are clear before a fresh acquisition.'}),
                (ConservationRefusal('capture_changed'),{'error':'capture_changed'})):
            with self.subTest(family=type(failure).__name__):
                self.assertEqual(self.refused(failure),expected)

    def test_unexpected_and_malformed_refusals_do_not_disclose_private_text(self):
        class HelperRefusal(Exception):
            def __str__(self):
                raise AssertionError('unexpected helper exception must not be formatted')

        class ForeignAcquisitionRefusal(acquire.AcquisitionRefusal):
            pass

        private='/private/source/helper.py secret=fixture\nprivate record body'
        for failure in (RuntimeError(private),OSError(private),HelperRefusal(private),
                ForeignAcquisitionRefusal('private_helper_token'),
                acquire.AcquisitionRefusal(private),BindingRefusal('source_code',private),
                FrameRefusal(''),ConservationRefusal('x'*65),
                acquire.transfer.Refusal(private,body=private)):
            with self.subTest(family=type(failure).__name__):
                self.assertEqual(self.refused(failure),{'error':'source_acquisition_refused'})


class OfflineSnapshotBoundsTests(unittest.TestCase):
    """All executable source/provider boundaries are fixtures, never real helpers."""
    def setUp(self):
        self.owned = tempfile.TemporaryDirectory(); self.addCleanup(self.owned.cleanup)
        root = Path(self.owned.name)
        self.source = root/'trusted'
        self.package = self.source/'dev-tools/pr_review'; self.package.mkdir(parents=True)
        common = root/'handoff'; common.mkdir(mode=0o700)
        captures = common/'firemud'; captures.mkdir(mode=0o700)
        self.snapshot = captures/'pr-review-stack.sqlite3'
        self.snapshot.write_bytes(b'sealed synthetic snapshot'); self.snapshot.chmod(0o600)
        plan = root/'plan.json'
        plan.write_bytes(acquire.transfer.canonical(dict(version=1, repository='owner/project', repository_id=17,
            default_base_ref='main', paused=True, scratch_git_writes=True,
            prs=[dict(pull_number=7, channels=['cli'])])))
        plan.chmod(0o600)
        self.args = types.SimpleNamespace(source_root=str(self.source), common=str(common),
            snapshot=str(self.snapshot), git_root=str(root/'proof-clone'), plan=str(plan),
            output=str(root/'frame.json'), provenance=str(root/'provenance.json'))
        stack = ExitStack(); self.addCleanup(stack.close)
        module = types.SimpleNamespace(__file__=str(self.package/'fixture.py'), parse_repo=lambda _: None,
                                       activate_hosted_preflight_budget=lambda *_args, **_kwargs: nullcontext())
        self.source_github = module
        stack.enter_context(patch.object(acquire.transfer, 'load_helpers', return_value=({'policy': object()}, {}, 'a'*64)))
        stack.enter_context(patch.object(acquire.transfer, 'source_hash', return_value=(None, {}, 'a'*64)))
        stack.enter_context(patch.object(acquire.importlib, 'import_module', return_value=module))
        stack.enter_context(patch.object(acquire, 'bind_source', return_value=nullcontext((object(), object()))))
        stack.enter_context(patch.object(acquire, 'scratch_git', return_value=root/'proof-clone'))
        stack.enter_context(patch.object(acquire, 'repository_matches'))
        stack.enter_context(patch.object(acquire, 'construct', return_value=object()))
        def frame(*_args, **kwargs):
            return dict(version=1, repository_id=17, snapshot_sha256=kwargs['snapshot_sha256'],
                        projection_complete=True, quiescent=True, obligations_clear=True,
                        prs=[dict(pull_number=7, channels={'cli': []})])
        self.frame = stack.enter_context(patch.object(acquire, 'acquire_frame', side_effect=frame))

    def test_exact_source_deadline_is_safe_fixed_refusal_without_publication(self):
        class Deadline(TimeoutError):
            def __str__(self):
                raise AssertionError('private source diagnostic must not be formatted')
        self.source_github.HostedPreflightDeadlineExceeded = Deadline
        self.frame.side_effect = Deadline('PRIVATE provider body')
        with self.assertRaises(acquire.AcquisitionRefusal) as failure:
            acquire.acquire(self.args)
        self.assertEqual(failure.exception.args, ('source_preflight_deadline_exceeded',))
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())
        self.assertEqual(self.snapshot.read_bytes(), b'sealed synthetic snapshot')

    def test_foreign_deadline_subclass_remains_unclassified(self):
        class Deadline(TimeoutError):
            pass
        class ForeignDeadline(Deadline):
            pass
        self.source_github.HostedPreflightDeadlineExceeded = Deadline
        self.frame.side_effect = ForeignDeadline('PRIVATE provider body')
        with self.assertRaises(ForeignDeadline):
            acquire.acquire(self.args)
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())

    def test_explicit_database_bound_exact_retry_preserves_binding_and_outputs(self):
        with self.snapshot.open('ab') as handle:
            handle.truncate((33 << 20)+1)
        with self.assertRaises(acquire.transfer.Refusal) as failure:
            acquire.acquire(self.args)
        self.assertEqual(failure.exception.code, 'input_file_invalid')
        self.frame.assert_not_called()
        self.args.max_database_bytes = 64 << 20
        self.assertEqual(acquire.acquire(self.args)['outcome'], 'created')
        before = Path(self.args.output).read_bytes(), Path(self.args.provenance).read_bytes()
        self.frame.reset_mock()
        self.args.max_database_bytes = 512 << 20
        result = acquire.acquire(self.args)
        self.assertTrue(result['replayed']); self.assertEqual(result['outcome'], 'unchanged')
        self.frame.assert_not_called()
        self.assertEqual(before, (Path(self.args.output).read_bytes(), Path(self.args.provenance).read_bytes()))

    def test_default_capture_admission_preserves_exact_original_provenance_shape(self):
        self.assertEqual(acquire.acquire(self.args)['outcome'], 'created')
        frame_bytes = Path(self.args.output).read_bytes()
        request = {name: getattr(self.args, name) for name in
                   ('source_root', 'common', 'snapshot', 'git_root', 'plan', 'output', 'provenance')}
        roots = (Path(self.args.common)/'firemud', Path(self.args.common)/'coderabbit-review-logs')
        manifest = conservation.inventory(roots, admitted_snapshot=self.snapshot)
        expected = dict(version=1, kind='source_owner_frame_acquisition', request=request,
            source_helpers={}, retained_frame=json.loads(frame_bytes), source_writer_pause_asserted=True,
            durable_writer_fence_established=False, source_obligations_audited=True, scratch_git_writes_permitted=True,
            binding=dict(snapshot_sha256=acquire.transfer.digest(self.snapshot.read_bytes()),
                source_code_sha256='a'*64, capture_inventory_sha256=acquire.transfer.digest(acquire.transfer.canonical(manifest)),
                plan_sha256=acquire.transfer.digest(Path(self.args.plan).read_bytes()),
                frame_sha256=acquire.transfer.digest(frame_bytes)))
        self.assertEqual(Path(self.args.provenance).read_bytes(), acquire.transfer.canonical(expected))
        self.assertNotIn('capture_limits', expected['request'])
        self.args.max_capture_bytes = 128 << 20
        with self.assertRaises(acquire.AcquisitionRefusal) as failure:
            acquire.acquire(self.args)
        self.assertEqual(failure.exception.args, ('acquisition_replay_conflict',))

    def test_explicit_caps_bind_replay_and_all_inventory_passes(self):
        self.args.max_capture_bytes, self.args.max_capture_entries = 768 << 20, 50000
        original_snapshot = self.snapshot.read_bytes()
        with patch.object(conservation, 'inventory', wraps=conservation.inventory) as inventory:
            self.assertEqual(acquire.acquire(self.args)['outcome'], 'created')
            retained = Path(self.args.output).read_bytes(), Path(self.args.provenance).read_bytes()
            proof = json.loads(retained[1])
            self.assertEqual(proof['request']['capture_limits'], dict(max_bytes=768 << 20, max_entries=50000))
            self.frame.reset_mock()
            result = acquire.acquire(self.args)
            self.assertTrue(result['replayed'])
            self.assertEqual(result['outcome'], 'unchanged')
            self.assertEqual(inventory.call_count, 4)
            for call in inventory.call_args_list:
                self.assertEqual(call.kwargs, dict(admitted_snapshot=self.snapshot, max_bytes=768 << 20, max_entries=50000))
        self.frame.assert_not_called()
        for name, value in (('max_capture_bytes', (768 << 20)+1), ('max_capture_entries', 50001),
                            ('max_capture_bytes', None), ('max_capture_entries', None)):
            previous = getattr(self.args, name)
            setattr(self.args, name, value)
            with self.subTest(name=name, value=value), self.assertRaises(acquire.AcquisitionRefusal) as failure:
                acquire.acquire(self.args)
            self.assertEqual(failure.exception.args, ('acquisition_replay_conflict',))
            setattr(self.args, name, previous)
        self.assertEqual((Path(self.args.output).read_bytes(), Path(self.args.provenance).read_bytes()), retained)
        self.assertEqual(self.snapshot.read_bytes(), original_snapshot)
        self.frame.assert_not_called()

    def test_replay_caps_require_exact_integer_types(self):
        self.args.max_capture_bytes, self.args.max_capture_entries = 1, 2
        acquire.acquire(self.args)
        proof_path = Path(self.args.provenance)
        original_proof = proof_path.read_bytes()
        original_frame = Path(self.args.output).read_bytes()
        self.frame.reset_mock()
        for name, value in (('max_bytes', True), ('max_bytes', 1.0), ('max_entries', 2.0)):
            proof = json.loads(original_proof)
            proof['request']['capture_limits'][name] = value
            proof_path.write_bytes(acquire.transfer.canonical(proof))
            with self.subTest(name=name, value=value), self.assertRaises(acquire.AcquisitionRefusal) as failure:
                acquire.acquire(self.args)
            self.assertEqual(failure.exception.args, ('acquisition_replay_conflict',))
            self.assertEqual(Path(self.args.output).read_bytes(), original_frame)
            proof_path.write_bytes(original_proof)
        self.frame.assert_not_called()

    def test_capture_caps_refuse_before_git_provider_or_frame_and_allow_exact_boundary(self):
        capture = self.snapshot.parent/'capture'
        capture.write_bytes(b'12345')
        for byte_cap, entry_cap in ((4, 3), (5, 2)):
            self.args.max_capture_bytes, self.args.max_capture_entries = byte_cap, entry_cap
            with self.subTest(bytes=byte_cap, entries=entry_cap), \
                    patch.object(acquire, 'scratch_git', side_effect=AssertionError('no Git proof')) as git, \
                    patch.object(acquire, 'repository_matches', side_effect=AssertionError('no provider')) as provider, \
                    self.assertRaises(ConservationRefusal) as failure:
                acquire.acquire(self.args)
            self.assertEqual(failure.exception.args, ('capture_inventory_too_large',))
            git.assert_not_called()
            provider.assert_not_called()
            self.frame.assert_not_called()
            self.assertFalse(Path(self.args.output).exists())
            self.assertFalse(Path(self.args.provenance).exists())
        self.args.max_capture_bytes, self.args.max_capture_entries = 5, 3
        self.assertEqual(acquire.acquire(self.args)['outcome'], 'created')
        frame = json.loads(Path(self.args.output).read_bytes())
        self.assertEqual(set(frame), {'version', 'repository_id', 'snapshot_sha256', 'projection_complete', 'quiescent', 'obligations_clear', 'prs'})

    def test_explicit_capture_cap_admits_large_unselected_artifact_without_default_change(self):
        folder = self.snapshot.parent/'unselected'
        folder.mkdir()
        capture = folder/'capture'
        size = (128 << 20)+1
        with capture.open('wb') as handle:
            handle.truncate(size)
        capture.chmod(0o600)
        with self.assertRaises(ConservationRefusal) as failure:
            acquire.acquire(self.args)
        self.assertEqual(failure.exception.args, ('capture_inventory_too_large',))
        self.frame.assert_not_called()
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())
        self.args.max_capture_bytes = size
        manifests = []
        original = conservation.inventory
        def tracked(*args, **kwargs):
            result = original(*args, **kwargs)
            manifests.append(result)
            return result
        with patch.object(conservation, 'inventory', side_effect=tracked):
            self.assertEqual(acquire.acquire(self.args)['outcome'], 'created')
        self.assertEqual(len(manifests), 2)
        for manifest in manifests:
            self.assertEqual(manifest['0/unselected/capture'][0], 'file')
            self.assertEqual(manifest['0/unselected/capture'][1][4], size)
            self.assertEqual(len(manifest['0/unselected/capture'][2]), 64)
        self.assertEqual(manifests[0], manifests[1])
        proof = json.loads(Path(self.args.provenance).read_bytes())
        self.assertEqual(proof['request']['capture_limits'], dict(max_bytes=size, max_entries=20000))
        self.assertEqual(proof['binding']['capture_inventory_sha256'],
                         acquire.transfer.digest(acquire.transfer.canonical(manifests[0])))
        self.assertNotIn('max_capture_bytes', self.frame.call_args.kwargs)
        self.assertNotIn('max_capture_entries', self.frame.call_args.kwargs)

    def test_final_inventory_uses_explicit_caps_before_publication(self):
        capture = self.snapshot.parent/'capture'
        capture.write_bytes(b'12345')
        self.args.max_capture_bytes, self.args.max_capture_entries = 5, 3
        original = self.frame.side_effect
        def changed(*args, **kwargs):
            frame = original(*args, **kwargs)
            capture.write_bytes(b'123456')
            return frame
        self.frame.side_effect = changed
        with self.assertRaises(ConservationRefusal) as failure:
            acquire.acquire(self.args)
        self.assertEqual(failure.exception.args, ('capture_inventory_too_large',))
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())

    def test_changed_snapshot_or_sidecar_prevents_publication(self):
        for mutation in ('digest', 'sidecar'):
            with self.subTest(mutation=mutation):
                def changed(*_args, **_kwargs):
                    if mutation == 'digest':
                        self.snapshot.write_bytes(b'changed synthetic snapshot')
                    else:
                        Path(str(self.snapshot)+'-wal').write_bytes(b'new sidecar')
                    raise OSError('private synthetic failure')
                self.frame.side_effect = changed
                with self.assertRaises((acquire.transfer.Refusal, ConservationRefusal)):
                    acquire.acquire(self.args)
                self.assertFalse(Path(self.args.output).exists())
                self.assertFalse(Path(self.args.provenance).exists())



class AcquisitionTests(unittest.TestCase):
    def setUp(self):
        import test_source_acquisition_frame as frame_fixture
        from test_source_acquisition_bindings import HELPERS
        self.frame_fixture,self.helpers=frame_fixture,HELPERS
        self.owned=tempfile.TemporaryDirectory()
        self.addCleanup(self.owned.cleanup)
        self.root=Path(self.owned.name)
        self.common=self.root/'handoff';self.common.mkdir(mode=0o700)
        logical=self.common/'firemud/pr-review-stack.json';logical.mkdir(parents=True,mode=0o700)
        self.database=logical.with_suffix('.sqlite3')
        self.helpers['sqlite_store'].SqliteStateStore(self.database).update(
            lambda _: self.helpers['state'].ReviewState(ordered_prs=(7,)))
        self.helpers['sqlite_review_records'].SqliteReviewRecords(self.database).bootstrap()
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
        return self.frame_fixture.FrameTests().fixture(store,records)[0]

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
