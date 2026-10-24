# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Pure carried-frame consistency proof; no source collection or native I/O."""
from copy import deepcopy
import hashlib
import json
import socket
import subprocess
import unittest
from unittest.mock import patch

import source_carried_frame as carried


SNAPSHOT = 'a' * 64


def original(key, kind, affected):
    return dict(source_key=key, kind=kind, affected_pull_numbers=affected,
                summary='Exact inert source text\nwith retained detail.',
                evidence=['source:' + key], original_status='pending')


def frame():
    finding = dict(pr=7, channel='cli', checkpoint='source-checkpoint-1',
                   completed=True, attributable=True, anchored=True, accepted=1,
                   held=True, finding_only_hold=True, source_resolution_status='finding_pending')
    result = dict(pr=7, channel='hosted', checkpoint='trigger-uncheckpointed:201',
                  completed=False, attributable=False, anchored=False,
                  held=True, finding_only_hold=False,
                  reason='Exact inert source result awaiting source adjudication')
    safe = dict(pr=8, channel='cli', checkpoint='safe-checkpoint', accepted=0,
                completed=True, attributable=True, anchored=True,
                source_resolution_status='resolved', provider_metadata={'original': [1, 'inert']})
    return dict(version=2, repository_id=17, snapshot_sha256=SNAPSHOT,
                projection_complete=True, quiescent=True, obligations_clear=False,
                prs=[dict(pull_number=7, channels={'cli': [finding], 'hosted': [result]}),
                     dict(pull_number=8, channels={'cli': [safe]})],
                obligations=[original('finding:one', 'finding', [7]),
                             original('result:hosted:one', 'result', [7]),
                             original('route:one', 'route', [7, 8])],
                bindings=[dict(pull_number=7, channel='cli', row_sha256=carried.row_digest(finding),
                               source_keys=['finding:one']),
                          dict(pull_number=7, channel='hosted', row_sha256=carried.row_digest(result),
                               source_keys=['result:hosted:one'])],
                deferred_controls=[dict(pull_number=7, channel='hosted',
                                        result_source_keys=['result:hosted:one'])])


class CarriedFrameTests(unittest.TestCase):
    def setUp(self):
        self.frame = frame()

    def validate(self):
        return carried.validate(self.frame, 17, SNAPSHOT)

    def refuse(self, code='carried_frame_invalid', callback=None):
        with self.assertRaises(carried.CarriedFrameRefusal) as failure:
            (callback or self.validate)()
        self.assertEqual(failure.exception.code, code)
        self.assertEqual(str(failure.exception), code)
        return failure.exception

    def selected(self, pr=8, channel='cli'):
        return carried.selected_history(self.frame,
            dict(repository_id=17, pull_number=pr, channel=channel), SNAPSHOT)

    def rebind(self, index):
        entry = self.frame['bindings'][index]
        for pull in self.frame['prs']:
            if pull['pull_number'] == entry['pull_number']:
                entry['row_sha256'] = carried.row_digest(pull['channels'][entry['channel']][0])

    def test_safe_and_carried_histories_return_same_objects_and_raw_values(self):
        before = deepcopy(self.frame)
        value = self.validate()
        self.assertEqual(self.frame, before)
        for pull in self.frame['prs']:
            for channel, history in pull['channels'].items():
                self.assertIs(value['histories'][(pull['pull_number'], channel)], history)
        self.assertIs(self.selected(), self.frame['prs'][1]['channels']['cli'])
        self.assertIs(self.selected(7), self.frame['prs'][0]['channels']['cli'])
        self.assertIs(value['originals']['result:hosted:one'], self.frame['obligations'][1])
        held = self.frame['prs'][0]['channels']['hosted'][0]
        self.assertEqual((held['completed'], held['attributable'], held['anchored']), (False, False, False))
        self.assertNotIn('accepted', held)
        self.assertEqual(value['deferred'], {(7, 'hosted'): ['result:hosted:one']})
        self.refuse('controls_deferred', lambda: self.selected(7, 'hosted'))

    def test_canonical_digest_binds_entire_unchanged_row_not_count_or_identity_subset(self):
        row = self.frame['prs'][0]['channels']['cli'][0]
        expected = hashlib.sha256((json.dumps(row, sort_keys=True, ensure_ascii=True,
            separators=(',', ':'), allow_nan=False) + '\n').encode()).hexdigest()
        self.assertEqual(carried.row_digest(row), expected)
        for field, value in (('checkpoint', 'same-count-wrong-checkpoint'),
                             ('reason', 'same-count-wrong-original-text'),
                             ('accepted', 2), ('anchored', False), ('pr', 8), ('channel', 'hosted')):
            with self.subTest(field=field):
                self.setUp()
                self.frame['prs'][0]['channels']['cli'][0][field] = value
                self.refuse()
        self.setUp()
        # Replacing one row by another with equal counters cannot reuse proof.
        self.frame['prs'][0]['channels']['cli'][0] = dict(row, checkpoint='other-row')
        self.refuse()

    def test_binding_requires_exact_row_pr_channel_hash_and_affected_original(self):
        for change in (dict(pull_number=8), dict(channel='hosted'),
                       dict(row_sha256='b' * 64), dict(row_sha256=SNAPSHOT.upper()),
                       dict(source_keys=['finding:missing']), dict(source_keys=['route:one']),
                       dict(source_keys=[]), dict(source_keys=['finding:one', 'finding:one'])):
            with self.subTest(change=change):
                self.setUp()
                self.frame['bindings'][0].update(change)
                self.refuse()
        self.setUp()
        self.frame['obligations'][0]['affected_pull_numbers'] = [8]
        self.refuse()

    def test_finding_only_hold_needs_exact_finding_binding_even_without_held_flag(self):
        for held in (False, None, 'absent'):
            with self.subTest(held=held):
                self.setUp()
                row = self.frame['prs'][0]['channels']['cli'][0]
                if held == 'absent':
                    del row['held']
                else:
                    row['held'] = held
                row['source_resolution_status'] = 'resolved'
                self.frame['bindings'].pop(0)
                self.refuse()

    def test_pending_accepted_rows_need_binding_and_unknown_status_never_waived(self):
        for status in (None, 'pending', 'unknown', '', True, [], {}):
            with self.subTest(status=status):
                self.setUp()
                row = self.frame['prs'][0]['channels']['cli'][0]
                row['source_resolution_status'] = status
                self.rebind(0)
                self.refuse()
        self.setUp()
        row = self.frame['prs'][0]['channels']['cli'][0]
        row.update(held=False, finding_only_hold=False)
        self.frame['bindings'].pop(0)
        self.refuse()
        self.setUp()
        del self.frame['prs'][0]['channels']['cli'][0]['source_resolution_status']
        self.rebind(0)
        self.refuse()

    def test_unsafe_flags_refuse_even_with_matching_binding_and_deferred_result(self):
        for flag in ('active_review', 'active_reservation', 'ambiguous', 'terminal_ambiguous',
                     'unstable', 'unreconciled', 'parent_moved', 'over_ceiling',
                     'rate_limited', 'actionable'):
            for index, channel in ((0, 'cli'), (1, 'hosted')):
                with self.subTest(flag=flag, channel=channel):
                    self.setUp()
                    self.frame['prs'][0]['channels'][channel][0][flag] = True
                    self.rebind(index)
                    self.refuse()

    def test_result_rows_require_result_binding_and_matching_deferred_channel(self):
        for mutation in ('missing-binding', 'finding-binding', 'missing-defer', 'wrong-channel',
                         'wrong-result-key', 'other-affected-pr'):
            with self.subTest(mutation=mutation):
                self.setUp()
                if mutation == 'missing-binding':
                    self.frame['bindings'].pop(1)
                elif mutation == 'finding-binding':
                    self.frame['bindings'][1]['source_keys'] = ['finding:one']
                elif mutation == 'missing-defer':
                    self.frame['deferred_controls'].clear()
                elif mutation == 'wrong-channel':
                    self.frame['deferred_controls'][0]['channel'] = 'cli'
                elif mutation == 'wrong-result-key':
                    self.frame['deferred_controls'][0]['result_source_keys'] = ['finding:one']
                else:
                    self.frame['obligations'][1]['affected_pull_numbers'] = [8]
                self.refuse()

    def test_result_ledger_itself_requires_defer_even_without_held_history(self):
        self.frame['prs'][0]['channels']['hosted'].clear()
        self.frame['bindings'].pop(1)
        self.frame['deferred_controls'].clear()
        self.refuse()

    def test_result_defer_survives_absence_of_finding_ledger_independently(self):
        result_row = self.frame['prs'][0]['channels']['hosted'][0]
        result_original = self.frame['obligations'][1]
        self.frame['prs'][0]['channels']['cli'].clear()
        self.frame['bindings'].pop(0)
        self.frame['obligations'].pop(0)
        self.validate()
        self.assertIs(result_row, self.frame['prs'][0]['channels']['hosted'][0])
        self.assertIs(result_original, self.frame['obligations'][0])
        self.refuse('controls_deferred', lambda: self.selected(7, 'hosted'))
        self.assertEqual(self.selected(7), [])

    def test_duplicate_pull_channel_ledger_keys_bindings_and_deferred_refuse(self):
        for collection in ('prs', 'obligations', 'bindings', 'deferred_controls'):
            with self.subTest(collection=collection):
                self.setUp()
                self.frame[collection].append(deepcopy(self.frame[collection][0]))
                self.refuse()
        self.setUp()
        # The same PR split across disjoint channel objects is also duplicate.
        hosted = self.frame['prs'][0]['channels'].pop('hosted')
        self.frame['prs'].append(dict(pull_number=7, channels={'hosted': hosted}))
        self.refuse()
        self.setUp()
        self.frame['prs'][0]['channels']['cli'].append(deepcopy(self.frame['prs'][0]['channels']['cli'][0]))
        self.refuse()

    def test_original_native_bounds_exact_shape_and_scope(self):
        for change in (dict(source_key='bad\nkey'), dict(source_key='x' * 201),
                       dict(kind='unknown'), dict(original_status='resolved'),
                       dict(summary=''), dict(summary='x' * ((16 << 10) + 1)),
                       dict(evidence=['x' * 1025]), dict(evidence=['x'] * 11),
                       dict(affected_pull_numbers=[True]), dict(affected_pull_numbers=[0]),
                       dict(affected_pull_numbers=[2**31]), dict(affected_pull_numbers=[99]),
                       dict(affected_pull_numbers=[7, 7]), dict(affected_pull_numbers=[8, 7]),
                       dict(extra='forged receipt')):
            with self.subTest(change=change):
                self.setUp()
                self.frame['obligations'][0].update(change)
                self.refuse()
        self.setUp()
        self.frame['obligations'][0]['summary'] = '<' * 6000
        self.refuse()  # Native HTML escaping exceeds 32 KiB despite small text.

    def test_large_ledger_is_not_mistaken_for_one_native_batch(self):
        self.frame['obligations'].extend(original(f'route:{index}', 'route', [7]) for index in range(997))
        self.assertEqual(len(self.validate()['originals']), 1000)
        self.frame['obligations'].append(original('route:extra', 'route', [7]))
        self.refuse()
        self.setUp()
        self.frame['obligations'].clear()
        self.refuse()

    def test_pull_and_history_row_collection_bounds_remain_independent(self):
        self.frame['prs'].extend(dict(pull_number=number, channels={'cli': []})
                                 for number in range(9, 107))
        self.assertEqual(len(self.frame['prs']), 100)
        self.validate()
        self.frame['prs'].append(dict(pull_number=107, channels={'cli': []}))
        self.refuse()
        self.setUp()
        history = [dict(pr=8)] * 10000
        self.frame['prs'][1]['channels']['cli'] = history
        self.assertIs(self.selected(), history)
        history.append(dict(pr=8))
        self.refuse()

    def test_frame_encoded_byte_budget_exact_boundary(self):
        row = self.frame['prs'][1]['channels']['cli'][0]
        row['inert_padding'] = ''
        padding = (1 << 20) - len(carried.canonical(self.frame))
        row['inert_padding'] = 'x' * padding
        self.assertEqual(len(carried.canonical(self.frame)), 1 << 20)
        self.validate()
        row['inert_padding'] += 'x'
        self.refuse()

    def test_scope_snapshot_version_flags_and_extra_fields_refuse(self):
        for change in (dict(version=1), dict(version=True), dict(repository_id=True),
                       dict(repository_id=18), dict(snapshot_sha256='b' * 64),
                       dict(projection_complete=False), dict(quiescent=1),
                       dict(obligations_clear=True), dict(extra='private extra')):
            with self.subTest(change=change):
                self.setUp()
                self.frame.update(change)
                self.refuse()
        for repository, snapshot in ((True, SNAPSHOT), (0, SNAPSHOT), (2**63, SNAPSHOT),
                                     (17, SNAPSHOT.upper()), (17, None)):
            with self.subTest(repository=repository, snapshot=snapshot):
                self.setUp()
                self.refuse(callback=lambda: carried.validate(self.frame, repository, snapshot))
        self.setUp()
        v1 = {key: value for key, value in self.frame.items()
              if key not in ('obligations', 'bindings', 'deferred_controls')}
        v1.update(version=1, obligations_clear=True)
        self.refuse(callback=lambda: carried.validate(v1, 17, SNAPSHOT))

    def test_malformed_types_are_fixed_refusals_not_private_python_exceptions(self):
        for key in ('prs', 'obligations', 'bindings', 'deferred_controls'):
            for value in (None, True, 1, 'private string', {}, ()):
                with self.subTest(key=key, value=value):
                    self.setUp()
                    self.frame[key] = value
                    self.refuse()
        for path, value in (
            (('prs', 0), None), (('prs', 0, 'channels'), []),
            (('prs', 0, 'channels', 'cli'), [None]),
            (('prs', 0, 'channels', 'cli', 0, 'accepted'), True),
            (('prs', 0, 'channels', 'cli', 0, 'accepted'), -1),
            (('prs', 0, 'channels', 'cli', 0, 'accepted'), 2**63),
            (('prs', 0, 'channels', 'cli', 0, 'held'), 1),
            (('obligations', 0, 'source_key'), []),
            (('obligations', 0, 'kind'), []),
            (('obligations', 0, 'affected_pull_numbers'), [7, {}]),
            (('bindings', 0, 'channel'), []),
            (('bindings', 0, 'source_keys'), [[]]),
            (('deferred_controls', 0, 'channel'), []),
            (('deferred_controls', 0, 'result_source_keys'), [[]]),
            (('prs', 1, 'channels', 'cli', 0, 'metadata'), float('nan')),
            (('prs', 1, 'channels', 'cli', 0, 'metadata'), object()),
        ):
            with self.subTest(path=path):
                self.setUp()
                target = self.frame
                for step in path[:-1]:
                    target = target[step]
                target[path[-1]] = value
                self.refuse()
        self.setUp()
        for selection in (None, [], {}, dict(repository_id=17),
                          dict(repository_id=17, pull_number=7.0, channel='cli'),
                          dict(repository_id=17, pull_number=7, channel=[]),
                          dict(repository_id=17, pull_number=True, channel='cli')):
            with self.subTest(selection=selection):
                self.refuse(callback=lambda: carried.selected_history(self.frame, selection, SNAPSHOT))

    def test_missing_selection_and_deferred_selection_never_return_numeric_inputs(self):
        for pr, channel in ((9, 'cli'), (8, 'hosted'), (7, 'unknown')):
            with self.subTest(pr=pr, channel=channel):
                self.refuse(callback=lambda: self.selected(pr, channel))
        self.refuse('controls_deferred', lambda: self.selected(7, 'hosted'))
        self.assertIs(self.selected(8, 'cli'), self.frame['prs'][1]['channels']['cli'])

    def test_validation_has_no_file_network_subprocess_or_native_effects(self):
        before = deepcopy(self.frame)
        with patch('builtins.open', side_effect=AssertionError('no files')), \
                patch('os.open', side_effect=AssertionError('no files')), \
                patch.object(socket, 'socket', side_effect=AssertionError('no network')), \
                patch.object(subprocess, 'run', side_effect=AssertionError('no subprocess')), \
                patch.object(subprocess, 'Popen', side_effect=AssertionError('no subprocess')):
            self.validate()
            self.selected()
        self.assertEqual(self.frame, before)


if __name__ == '__main__':
    unittest.main()
