# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Offline retained-preview consistency proof, without native attestation."""
from copy import deepcopy
import hashlib
import json
import unittest

from source_carried_binding import reference
from source_carried_frame import CarriedFrameRefusal
from test_source_carried_frame import SNAPSHOT, frame, original


def native_body(batch):
    """Independent fixture encoding in the public Go model struct field order."""
    source = {key: batch['source'][key] for key in ('namespace', 'repository_id', 'snapshot_sha256')}
    obligations = [{key: row[key] for key in ('source_key', 'kind', 'affected_pull_numbers',
                    'summary', 'evidence', 'original_status')}
                   for row in sorted(batch['obligations'], key=lambda row: row['source_key'])]
    value = dict(version=batch['version'], source=source,
                 expected_revision=batch['expected_revision'], obligations=obligations)
    text = json.dumps(value, ensure_ascii=False, separators=(',', ':'), allow_nan=False)
    for character in '<>&\u2028\u2029':
        text = text.replace(character, '\\u' + format(ord(character), '04x'))
    return text.encode('utf-8')


def proof_for(frame_value, namespace='copied-source', *, revision=3, project='project_owned', size=100):
    batches = []
    for start in range(0, len(frame_value['obligations']), size):
        batch = dict(version=1, source=dict(namespace=namespace, repository_id=frame_value['repository_id'],
                     snapshot_sha256=frame_value['snapshot_sha256']), expected_revision=revision,
                     obligations=deepcopy(frame_value['obligations'][start:start + size]))
        selected = len(batch['obligations'])
        preview = dict(project_id=project, source_revision=revision, selected=selected, new=0,
                       unchanged=selected, payload_sha256=hashlib.sha256(native_body(batch)).hexdigest(),
                       preview_sha256='b' * 64)
        batches.append(dict(batch=batch, preview=preview))
    return dict(version=1, batches=batches)


class CarriedBindingTests(unittest.TestCase):
    def setUp(self):
        self.frame = frame()
        self.selection = dict(repository_id=17, source='copied-source')
        self.proof = proof_for(self.frame, size=2)

    def validate(self):
        return reference(self.frame, self.selection, SNAPSHOT, self.proof)

    def refuse(self):
        with self.assertRaises(CarriedFrameRefusal) as failure:
            self.validate()
        self.assertEqual(failure.exception.code, 'carried_frame_invalid')
        self.assertEqual(str(failure.exception), 'carried_frame_invalid')

    def test_complete_union_retained_previews_bind_one_current_source_revision(self):
        before = deepcopy((self.frame, self.selection, self.proof))
        self.assertEqual(self.validate(), dict(namespace='copied-source', snapshot_sha256=SNAPSHOT, revision=3))
        self.assertEqual((self.frame, self.selection, self.proof), before)
        # Receipt and object order are independent of native normalized ordering.
        self.proof['batches'].reverse()
        for item in self.proof['batches']:
            item['batch']['obligations'].reverse()
            item['batch'] = dict(reversed(list(item['batch'].items())))
            item['batch']['obligations'] = [dict(reversed(list(row.items())))
                                           for row in item['batch']['obligations']]
        self.assertEqual(self.validate()['revision'], 3)

    def test_native_html_unicode_and_struct_order_match_explicit_wire_bytes(self):
        self.selection['source'] = 'source<&é'
        self.frame['obligations'][0].update(summary='é😀 <tag>&\u2028\u2029\n', evidence=['"quoted" é'])
        self.proof = proof_for(self.frame, self.selection['source'], size=1)
        batch = self.proof['batches'][0]['batch']
        expected = ('{"version":1,"source":{"namespace":"source\\u003c\\u0026é",'
            '"repository_id":17,"snapshot_sha256":"' + SNAPSHOT + '"},"expected_revision":3,'
            '"obligations":[{"source_key":"finding:one","kind":"finding",'
            '"affected_pull_numbers":[7],"summary":"é😀 \\u003ctag\\u003e\\u0026\\u2028\\u2029\\n",'
            '"evidence":["\\\"quoted\\\" é"],"original_status":"pending"}]}').encode('utf-8')
        self.assertEqual(native_body(batch), expected)
        self.assertEqual(self.proof['batches'][0]['preview']['payload_sha256'], hashlib.sha256(expected).hexdigest())
        self.validate()
        self.proof['batches'][0]['preview']['payload_sha256'] = hashlib.sha256(
            json.dumps(batch, sort_keys=True).encode()).hexdigest()
        self.refuse()

    def test_namespace_repository_snapshot_revision_and_project_disagreement_refuse(self):
        for path, value in (
            (('batch', 'source', 'namespace'), 'other-source'),
            (('batch', 'source', 'repository_id'), 18),
            (('batch', 'source', 'repository_id'), True),
            (('batch', 'source', 'snapshot_sha256'), 'c' * 64),
            (('batch', 'expected_revision'), 4),
            (('preview', 'source_revision'), 4),
            (('preview', 'project_id'), 'another_project'),
        ):
            with self.subTest(path=path, value=value):
                self.setUp()
                target = self.proof['batches'][1]
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = value
                self.refuse()
        for epoch in (0, -1, True, 3.0, '3', None, 2**63 - 1):
            with self.subTest(epoch=epoch):
                self.setUp()
                for item in self.proof['batches']:
                    item['batch']['expected_revision'] = epoch
                    item['preview']['source_revision'] = epoch
                self.refuse()

    def test_pending_new_mapping_wrong_selected_or_unchanged_counts_refuse(self):
        for key, value in (('new', 1), ('new', True), ('unchanged', 1),
                           ('selected', 1), ('selected', 2.0), ('source_revision', True)):
            with self.subTest(key=key, value=value):
                self.setUp()
                self.proof['batches'][0]['preview'][key] = value
                self.refuse()

    def test_revision_matches_native_prepared_control_safe_integer_boundary(self):
        maximum = 2**53 - 1
        self.proof = proof_for(self.frame, revision=maximum)
        self.assertEqual(self.validate()['revision'], maximum)
        for epoch in (maximum + 1, 2**63 - 2):
            with self.subTest(epoch=epoch):
                self.proof = proof_for(self.frame, revision=epoch)
                self.refuse()

    def test_changed_original_text_evidence_kind_or_scope_never_matches_retained_union(self):
        for change in (dict(summary='different private text'), dict(evidence=['changed:proof']),
                       dict(kind='route'), dict(affected_pull_numbers=[8]), dict(extra='forged'),
                       dict(original_status='open'), dict(source_key='finding:missing')):
            with self.subTest(change=change):
                self.setUp()
                batch = self.proof['batches'][0]['batch']
                batch['obligations'][0].update(change)
                self.proof['batches'][0]['preview']['payload_sha256'] = hashlib.sha256(native_body(batch)).hexdigest()
                self.refuse()

    def test_missing_extra_and_duplicate_originals_across_batches_refuse(self):
        self.proof['batches'].pop()
        self.refuse()
        self.setUp()
        self.proof['batches'].append(deepcopy(self.proof['batches'][0]))
        self.refuse()
        self.setUp()
        batch = self.proof['batches'][1]['batch']
        batch['obligations'] = [original('route:extra', 'route', [7])]
        self.proof['batches'][1]['preview']['payload_sha256'] = hashlib.sha256(native_body(batch)).hexdigest()
        self.refuse()
        self.setUp()
        self.proof['batches'][0]['batch']['obligations'][1] = deepcopy(
            self.proof['batches'][0]['batch']['obligations'][0])
        self.refuse()

    def test_payload_digest_is_checked_and_preview_digest_is_shape_only(self):
        self.proof['batches'][0]['preview']['payload_sha256'] = 'c' * 64
        self.refuse()
        for key in ('payload_sha256', 'preview_sha256'):
            for value in (None, True, '', 'Z' * 64, 'a' * 63, 'a' * 65):
                with self.subTest(key=key, value=value):
                    self.setUp()
                    self.proof['batches'][0]['preview'][key] = value
                    self.refuse()
        self.setUp()
        self.proof['batches'][0]['preview']['preview_sha256'] = 'd' * 64
        self.validate()  # Offline assertion cannot authenticate a server MAC.

    def test_batch_and_proof_collection_limits_are_separate_from_frame_ledger(self):
        self.frame['obligations'].extend(original(f'route:extra:{n}', 'route', [7]) for n in range(997))
        self.proof = proof_for(self.frame, size=100)
        self.assertEqual(len(self.proof['batches']), 10)
        self.validate()
        self.proof = proof_for(self.frame, size=101)
        self.refuse()
        self.setUp()
        self.proof['batches'] *= 51
        self.assertEqual(len(self.proof['batches']), 102)
        self.refuse()
        self.proof['batches'].clear()
        self.refuse()

    def test_native_wrapper_budget_exact_boundary_includes_preview_field(self):
        self.frame['obligations'] = [original(f'route:{n:02}', 'route', [7]) for n in range(17)]
        # Remove held histories; all originals in this budget fixture are routes.
        self.frame['prs'][0]['channels'] = {'cli': []}
        self.frame['bindings'].clear()
        self.frame['deferred_controls'].clear()
        for row in self.frame['obligations']:
            row['summary'] = 'x' * 15000
        self.proof = proof_for(self.frame)
        batch = self.proof['batches'][0]['batch']
        overhead = len(b'{"batch":,"preview_sha256":"' + b'0' * 64 + b'"}')
        padding = (256 << 10) - len(native_body(batch)) - overhead
        self.assertGreater(padding, 0)
        for row in self.frame['obligations']:
            extra = min(padding, (16 << 10) - len(row['summary']))
            row['summary'] += 'x' * extra
            padding -= extra
        self.assertEqual(padding, 0)
        self.proof = proof_for(self.frame)
        self.assertEqual(len(native_body(self.proof['batches'][0]['batch'])) + overhead, 256 << 10)
        self.validate()
        self.frame['obligations'][-1]['summary'] += 'x'
        self.proof = proof_for(self.frame)
        self.refuse()

    def test_malformed_types_and_private_objects_return_fixed_refusal(self):
        for value in (None, True, [], {'version': 1}, {'version': True, 'batches': []}):
            with self.subTest(value=value):
                self.setUp()
                self.proof = value
                self.refuse()
        for path, value in (
            (('batch',), None), (('preview',), []), (('batch', 'source'), []),
            (('batch', 'obligations'), {}), (('batch', 'obligations'), [None]),
            (('preview', 'project_id'), '/private/path'), (('preview', 'project_id'), []),
            (('batch', 'expected_revision'), object()), (('batch', 'extra'), 'private body'),
        ):
            with self.subTest(path=path):
                self.setUp()
                target = self.proof['batches'][0]
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = value
                self.refuse()
        for value in (None, [], {}, dict(repository_id=True, source='copied-source'),
                      dict(repository_id=17, source=[])):
            with self.subTest(selection=value):
                self.setUp()
                self.selection = value
                self.refuse()


if __name__ == '__main__':
    unittest.main()
