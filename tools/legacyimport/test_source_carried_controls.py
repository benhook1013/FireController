# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Carried control publication/replay proof using owned sealed inputs only."""
from contextlib import ExitStack, redirect_stderr, redirect_stdout
from copy import deepcopy
import io
import json
import os
from pathlib import Path
import sqlite3
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import test_prepare_source_controls as fixtures
from test_source_carried_binding import proof_for
from test_source_carried_frame import frame, original


adapter = fixtures.adapter
FACTS = dict(minimum_remaining=1, maximum_remaining=4, initial_dry=2,
             taper=3, tapered=False, closed=False)


class CarriedControlTests(unittest.TestCase):
    private = fixtures.LocalContractTests.private

    def setUp(self):
        fixtures.LocalContractTests.setUp(self)
        snapshot = self.root / 'source.sqlite3'
        with sqlite3.connect(snapshot) as connection:
            connection.execute('CREATE TABLE review_state(singleton INTEGER, state_json TEXT)')
            connection.execute("INSERT INTO review_state VALUES(1, '{}')")
        snapshot.chmod(0o600)
        self.frame = frame()
        self.frame['snapshot_sha256'] = adapter.digest(snapshot.read_bytes())
        self.proof = proof_for(self.frame, self.selected['source'])
        self.args = SimpleNamespace(source_root=str(self.root / 'trusted'), snapshot=str(snapshot),
            history_file=str(self.private('frame.json', adapter.canonical(self.frame))),
            selection=str(self.private('selection.json', adapter.canonical(self.selected))),
            obligations_file=str(self.private('obligations.json', adapter.canonical(self.proof))),
            output=str(self.root / 'prepared.json'), provenance=str(self.root / 'provenance.json'))
        self.store = SimpleNamespace(validate_controller_compatibility=lambda _: 1,
                                      load_validated_controller_state=lambda *args, **kwargs: object())
        self.facts = Mock(return_value=deepcopy(FACTS))

    def rewrite(self):
        self.private('frame.json', adapter.canonical(self.frame))
        self.private('selection.json', adapter.canonical(self.selected))
        self.private('obligations.json', adapter.canonical(self.proof))

    def invoke(self):
        with ExitStack() as stack:
            stack.enter_context(patch.object(adapter, 'load_helpers',
                return_value=({'sqlite_store': self.store}, {'owned.py': 'b' * 64}, 'a' * 64)))
            stack.enter_context(patch.object(adapter, 'source_hash', return_value=(None, {}, 'a' * 64)))
            stack.enter_context(patch.object(adapter, 'control_facts', self.facts))
            for name in ('subprocess.run', 'subprocess.Popen', 'socket.socket'):
                stack.enter_context(patch(name, side_effect=AssertionError('no external work')))
            return adapter.prepare(self.args)

    def refuse(self, code=None):
        with self.assertRaises(adapter.Refusal) as failure:
            self.invoke()
        if code is not None:
            self.assertEqual(failure.exception.code, code)
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())
        self.assertEqual(list(self.root.glob('.prepared-controls-*')), [])

    def test_v2_finite_control_retains_false_clearance_and_hash_bound_receipts_on_retry(self):
        inputs = {name: Path(getattr(self.args, name)).read_bytes()
                  for name in ('snapshot', 'history_file', 'selection', 'obligations_file')}
        first = self.invoke()
        prepared_raw, provenance_raw = Path(self.args.output).read_bytes(), Path(self.args.provenance).read_bytes()
        prepared, provenance = json.loads(prepared_raw), json.loads(provenance_raw)
        self.assertEqual(first['outcome'], 'created')
        self.assertEqual(prepared, dict(self.selected, version=2, **FACTS,
            obligations=dict(namespace='copied-source', snapshot_sha256=self.frame['snapshot_sha256'], revision=3),
            source_sha256=prepared['source_sha256']))
        self.assertEqual(provenance['binding'], dict(version=2, snapshot_sha256=self.frame['snapshot_sha256'],
            history_sha256=adapter.digest(inputs['history_file']), source_code_sha256='a' * 64,
            obligations_binding_sha256=adapter.digest(inputs['obligations_file'])))
        self.assertEqual(provenance['source_owner_assertions'],
                         dict(projection_complete=True, quiescent=True, obligations_clear=False))
        self.assertFalse(provenance['independently_verified_live_assertions'])
        self.assertEqual(prepared['source_sha256'], adapter.digest(adapter.canonical(provenance['binding'])))
        self.assertEqual(provenance['prepared_sha256'], adapter.digest(prepared_raw))
        self.assertEqual(provenance['selection_sha256'], adapter.digest(inputs['selection']))
        self.assertEqual(provenance['source_helpers'], {'owned.py': 'b' * 64})
        self.assertEqual(self.facts.call_args.args[3], self.frame['prs'][0]['channels']['cli'])
        self.assertTrue(self.facts.call_args.args[3][0]['finding_only_hold'])
        self.assertEqual(self.invoke()['outcome'], 'unchanged')
        self.assertEqual((Path(self.args.output).read_bytes(), Path(self.args.provenance).read_bytes()),
                         (prepared_raw, provenance_raw))
        for name, content in inputs.items():
            self.assertEqual(Path(getattr(self.args, name)).read_bytes(), content)
        self.assertFalse(any(Path(self.args.snapshot + suffix).exists() for suffix in ('-wal', '-shm', '-journal')))

    def test_v2_minimum_only_facts_keep_unbounded_flag_and_same_obligation_reference(self):
        self.facts.return_value = dict(FACTS, maximum_remaining=0, maximum_unbounded=True)
        self.invoke()
        prepared = json.loads(Path(self.args.output).read_bytes())
        self.assertEqual(prepared['version'], 2)
        self.assertTrue(prepared['maximum_unbounded'])
        self.assertEqual(prepared['maximum_remaining'], 0)
        self.assertEqual(prepared['minimum_remaining'], 1)
        self.assertEqual(prepared['obligations']['revision'], 3)

    def test_deferred_channel_refuses_before_binding_or_numeric_projection(self):
        self.selected.update(channel='hosted')
        self.rewrite()
        self.args.obligations_file = '/private/nonexistent-proof-not-read'
        self.refuse('controls_deferred')
        self.facts.assert_not_called()
        self.args.obligations_file = None
        self.refuse('controls_deferred')

    def test_numeric_v2_requires_binding_but_v1_refuses_even_unread_binding(self):
        self.args.obligations_file = None
        self.refuse('obligations_binding_required')
        self.facts.assert_not_called()
        self.set_v1()
        self.args.obligations_file = '/private/nonexistent-proof-not-read'
        self.refuse('obligations_binding_unexpected')

    def set_v1(self):
        self.frame = dict(version=1, repository_id=17, snapshot_sha256=self.frame['snapshot_sha256'],
            projection_complete=True, quiescent=True, obligations_clear=True,
            prs=[dict(pull_number=7, channels={'cli': []})])
        self.args.obligations_file = None
        self.private('frame.json', adapter.canonical(self.frame))

    def test_v1_without_binding_keeps_exact_preexisting_finite_bytes_and_provenance(self):
        self.set_v1()
        self.invoke()
        binding = dict(version=1, snapshot_sha256=self.frame['snapshot_sha256'],
                       history_sha256=adapter.digest(Path(self.args.history_file).read_bytes()),
                       source_code_sha256='a' * 64)
        expected = dict(self.selected, source_sha256=adapter.digest(adapter.canonical(binding)), **FACTS)
        self.assertEqual(Path(self.args.output).read_bytes(), adapter.canonical(expected))
        proof = json.loads(Path(self.args.provenance).read_bytes())
        self.assertEqual(proof['binding'], binding)
        self.assertNotIn('obligations', expected)
        self.assertNotIn('obligations_binding_sha256', proof['binding'])
        self.assertEqual(proof['source_owner_assertions']['obligations_clear'], True)

    def test_receipt_scope_and_union_mismatches_refuse_without_publication(self):
        for mutation in ('project', 'namespace', 'revision', 'snapshot', 'original', 'missing'):
            with self.subTest(mutation=mutation):
                self.setUp()
                self.proof = proof_for(self.frame, self.selected['source'], size=1)
                item = self.proof['batches'][1]
                if mutation == 'project':
                    item['preview']['project_id'] = 'wrong-project'
                elif mutation == 'namespace':
                    item['batch']['source']['namespace'] = 'other-source'
                elif mutation == 'revision':
                    item['preview']['source_revision'] = 4
                elif mutation == 'snapshot':
                    item['batch']['source']['snapshot_sha256'] = 'c' * 64
                elif mutation == 'original':
                    item['batch']['obligations'][0]['evidence'] = ['different:proof']
                else:
                    self.proof['batches'].pop()
                self.rewrite()
                self.refuse('carried_frame_invalid')
                self.facts.assert_not_called()

    def test_private_binding_input_type_size_duplicate_json_and_symlink_refuse(self):
        for case in ('permissions', 'symlink', 'relative', 'oversize', 'duplicate-json'):
            with self.subTest(case=case):
                self.setUp()
                path = Path(self.args.obligations_file)
                if case == 'permissions':
                    path.chmod(0o644)
                elif case == 'symlink':
                    link = self.root / 'proof-link'
                    link.symlink_to(path)
                    self.args.obligations_file = str(link)
                elif case == 'relative':
                    self.args.obligations_file = 'private-relative-proof'
                elif case == 'oversize':
                    path.write_bytes(b' ' * ((2 << 20) + 1))
                else:
                    path.write_bytes(b'{"version":1,"version":1,"batches":[]}')
                self.refuse()
                self.facts.assert_not_called()

    def test_binding_path_output_aliases_refuse_before_either_output_is_published(self):
        for destination in ('output', 'provenance'):
            with self.subTest(destination=destination):
                self.setUp()
                before = Path(self.args.obligations_file).read_bytes()
                setattr(self.args, destination, self.args.obligations_file)
                with self.assertRaises(adapter.Refusal) as failure:
                    self.invoke()
                self.assertEqual(failure.exception.code, 'output_invalid')
                self.assertEqual(Path(self.args.obligations_file).read_bytes(), before)
                other = 'output' if destination == 'provenance' else 'provenance'
                self.assertFalse(Path(getattr(self.args, other)).exists())

    def test_input_change_during_projection_and_helper_hash_change_prevent_publication(self):
        for name in ('snapshot', 'history_file', 'selection', 'obligations_file'):
            with self.subTest(name=name):
                self.setUp()
                path = Path(getattr(self.args, name))

                def change(*args):
                    path.write_bytes(path.read_bytes() + b' ')
                    return deepcopy(FACTS)

                self.facts.side_effect = change
                self.refuse('source_changed')
        self.setUp()
        # The final helper digest check runs before publication.
        with patch.object(adapter, 'load_helpers', return_value=({'sqlite_store': self.store}, {}, 'a' * 64)), \
                patch.object(adapter, 'source_hash', return_value=(None, {}, 'b' * 64)), \
                patch.object(adapter, 'control_facts', return_value=deepcopy(FACTS)):
            with self.assertRaises(adapter.Refusal) as failure:
                adapter.prepare(self.args)
        self.assertEqual(failure.exception.code, 'source_changed')
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())

    def test_changed_binding_bytes_cannot_silently_replay_different_assertion(self):
        self.invoke()
        before = Path(self.args.output).read_bytes(), Path(self.args.provenance).read_bytes()
        path = Path(self.args.obligations_file)
        path.write_bytes(path.read_bytes() + b' ')
        with self.assertRaises(adapter.Refusal) as failure:
            self.invoke()
        self.assertEqual(failure.exception.code, 'output_conflict')
        self.assertEqual((Path(self.args.output).read_bytes(), Path(self.args.provenance).read_bytes()), before)

    def test_cli_binding_flag_is_explicit_unique_unabbreviated_and_quiet(self):
        args = [value for name in ('source-root', 'snapshot', 'history-file', 'selection', 'output', 'provenance')
                for value in ('--' + name, '/private/' + name)]
        self.assertIsNone(adapter.parser().parse_args(args).obligations_file)
        self.assertEqual(adapter.parser().parse_args(args + ['--obligations-file', '/private/binding']).obligations_file,
                         '/private/binding')
        for tail in (['--obligations-file', '/private/PRIVATE', '--obligations-file', '/private/OTHER'],
                     ['--obligations-f', '/private/PRIVATE']):
            with self.subTest(tail=tail), redirect_stderr(io.StringIO()) as errors, self.assertRaises(SystemExit) as failure:
                adapter.parser().parse_args(args + tail)
            self.assertEqual(failure.exception.code, 2)
            self.assertEqual(errors.getvalue(), '{"error":"arguments_invalid"}\n')

    def test_public_command_creates_recovers_and_reports_deferred_without_private_text(self):
        def command():
            names = ('source_root', 'snapshot', 'history_file', 'selection', 'output', 'provenance', 'obligations_file')
            return [part for name in names for part in ('--' + name.replace('_', '-'), getattr(self.args, name))]

        with patch.object(adapter, 'load_helpers', return_value=({'sqlite_store': self.store}, {}, 'a' * 64)), \
                patch.object(adapter, 'source_hash', return_value=(None, {}, 'a' * 64)), \
                patch.object(adapter, 'control_facts', self.facts), \
                patch('subprocess.run', side_effect=AssertionError('no subprocess')), \
                patch('subprocess.Popen', side_effect=AssertionError('no subprocess')), \
                patch('socket.socket', side_effect=AssertionError('no network')):
            for expected in ('created', 'unchanged'):
                with redirect_stdout(io.StringIO()) as output, redirect_stderr(io.StringIO()) as errors:
                    status = adapter.main(command())
                self.assertEqual(status, 0)
                self.assertEqual(json.loads(output.getvalue())['outcome'], expected)
                self.assertEqual(errors.getvalue(), '')
                self.assertNotIn('Exact inert source text', output.getvalue())
                self.assertLess(len(output.getvalue()), 512)
            self.selected['channel'] = 'hosted'
            self.rewrite()
            self.args.output, self.args.provenance = str(self.root / 'deferred-control'), str(self.root / 'deferred-proof')
            with redirect_stdout(io.StringIO()) as output, redirect_stderr(io.StringIO()) as errors:
                status = adapter.main(command())
            self.assertEqual(status, 3)
            self.assertEqual(output.getvalue(), '')
            self.assertEqual(errors.getvalue(), '{"error": "controls_deferred"}\n')
            self.assertFalse(Path(self.args.output).exists())
            self.assertFalse(Path(self.args.provenance).exists())


@unittest.skipUnless(os.environ.get('FIRECONTROLLER_SOURCE_ROOT'), 'explicit trusted source root not selected')
class TrustedCarriedControlTests(unittest.TestCase):
    def fixture(self, *, unbounded=False):
        owned = fixtures.ActualSourceHelperTests('test_partial_cli_two_and_deterministic_sealed_output')
        owned.source_root = os.environ['FIRECONTROLLER_SOURCE_ROOT']
        owned.helpers, _, _ = adapter.load_helpers(owned.source_root)
        owned.setUp()
        self.addCleanup(owned.doCleanups)
        if unbounded:
            owned.allocation(min_additional_completed=2, max_additional_completed=None)
            history = [owned.timeline(), owned.review(1)]
        else:
            history = [owned.review(1), owned.review(2)]
        owned.inputs(history)
        source_frame = json.loads(owned.history_path.read_bytes())
        source_frame.update(version=2, obligations_clear=False,
            obligations=[original('route:retained', 'route', [7])], bindings=[], deferred_controls=[])
        owned.private('history.json', adapter.canonical(source_frame))
        owned.args.obligations_file = str(owned.private('retained-previews.json',
            adapter.canonical(proof_for(source_frame, owned.selected['source']))))
        return owned

    def test_canonical_finite_semantics_and_raw_source_preservation(self):
        owned = self.fixture()
        before = owned.snapshot.read_bytes(), owned.history_path.read_bytes()
        result, control = owned.run_preparation()
        self.assertEqual(result['outcome'], 'created')
        self.assertEqual({key: control[key] for key in FACTS}, FACTS)
        self.assertEqual(control['version'], 2)
        self.assertEqual(control['obligations']['revision'], 3)
        self.assertEqual((owned.snapshot.read_bytes(), owned.history_path.read_bytes()), before)
        self.assertEqual(owned.run_preparation()[0]['outcome'], 'unchanged')

    def test_canonical_minimum_only_semantics_remain_unbounded_with_binding(self):
        owned = self.fixture(unbounded=True)
        _, control = owned.run_preparation()
        self.assertEqual(control['minimum_remaining'], 1)
        self.assertEqual(control['maximum_remaining'], 0)
        self.assertTrue(control['maximum_unbounded'])
        self.assertEqual(control['initial_dry'], 1)
        self.assertFalse(control['tapered'])
        self.assertEqual(control['obligations']['revision'], 3)


if __name__ == '__main__':
    unittest.main()
