# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Synthetic offline verification; optional explicitly trusted source helpers."""
from contextlib import ExitStack
from copy import deepcopy
from datetime import datetime
import hashlib
import importlib
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from source_obligation_results import ResultRefusal, validate_unsettled_hosted


RUN, REPO, HEAD = 'owned-run', 'owner/project', 'a' * 40
START, FINISH = '2026-01-02T03:04:05Z', '2026-01-02T03:05:05Z'


def fixture():
    trigger = dict(databaseId=101, author={'login': 'owner'}, body='@coderabbitai full review',
                   createdAt=START, updatedAt=START, url='https://example.invalid/101')
    review = dict(databaseId=201, author={'login': 'coderabbitai[bot]'}, state='COMMENTED',
                  submittedAt=FINISH, commit={'oid': HEAD},
                  body='**Actionable comments posted: 4**', url='https://example.invalid/201')
    threads = []
    for index in range(4):
        comment = dict(databaseId=301 + index, author={'login': 'coderabbitai[bot]'},
                       body=f'**Fix owned issue {index}.**\n\nOwned fixture detail {index}.',
                       createdAt=FINISH, updatedAt=FINISH, originalCommit={'oid': HEAD},
                       pullRequestReview={'databaseId': 201})
        threads.append(dict(id=f'owned-thread-{index}', isResolved=False, isOutdated=False,
                            comments={'nodes': [comment]}))
    payload = {'data': {'repository': {'nameWithOwner': REPO, 'pullRequest': {
        'number': 7, 'headRefOid': HEAD, 'comments': {'nodes': [trigger]},
        'reviews': {'nodes': [review]}, 'reviewThreads': {'nodes': threads}}}}}
    record = dict(repository=REPO, pr_number=7, head_sha=HEAD, status='posted',
                  sqlite_attempt_id=RUN, anchor={'child_head': HEAD},
                  trigger=dict(id=101, created_at=START, url=trigger['url'],
                               author_login='owner', type='full', command=trigger['body']))
    metadata = dict(state='completed', terminal=True, attributable=True, repository=REPO,
                    pull_request=7, head_sha=HEAD, trigger_id=101, response_id=201,
                    observed_at=FINISH, trigger_record=record)
    run = dict(run_id=RUN, source_pr=7, channel='hosted', source_head=HEAD,
               outcome='completed', attributable=True, finalized=False, finalized_at=None,
               started_at=START, finished_at=FINISH,
               counts={'found': 4, 'accepted': 1, 'routed': 1})
    attempt = dict(attempt_id=RUN, run_id=RUN, channel='hosted', candidate_sha=HEAD,
                   state='completed', repository=REPO, trigger_id='101', provider_review_id='201',
                   started_at=START, finished_at=FINISH, checkpoint_id=None)
    return payload, metadata, dict(pr=7, runs=[run], attempts=[attempt], findings=[])


def synthetic_helpers():
    def complete(payload, pr):
        pull = payload['data']['repository']['pullRequest']
        if pull['number'] != pr:
            raise ValueError('private invalid PR')
        for name in ('comments', 'reviews', 'reviewThreads'):
            if pull[name].get('pageInfo', {}).get('hasNextPage'):
                raise ValueError('private incomplete')
            assert type(pull[name]['nodes']) is list
        return pull

    def state(repo, pr, payload, record, current_record_path=None, *, now=None):
        assert current_record_path is None
        pull = complete(payload, pr)
        review = pull['reviews']['nodes'][0]
        return SimpleNamespace(state='completed', terminal=True, attributed=True,
                               response_id=review['databaseId'], repository=repo, pr_number=pr,
                               head_sha=record['head_sha'], current_head_sha=pull['headRefOid'],
                               trigger_comment_id=record['trigger']['id'], trigger_created_at=START)

    def terminal(result, pull, *, observed_at):
        matches = [row for row in pull['reviews']['nodes'] if row['databaseId'] == result.response_id]
        assert len(matches) == 1
        return matches[0]['submittedAt'], matches[0], None

    def findings(result, pull, record, **kwargs):
        return tuple(SimpleNamespace(source_finding_key='hosted-comment:' + str(row['comments']['nodes'][0]['databaseId']),
                                     title=row['comments']['nodes'][0]['body'].splitlines()[0],
                                     detail=row['comments']['nodes'][0]['body'])
                     for row in pull['reviewThreads']['nodes'])

    return {
        'hosted': SimpleNamespace(trigger_state=state,
            parse_timestamp=lambda value: datetime.fromisoformat(value.replace('Z', '+00:00')),
            normalize_command=lambda body: ' '.join(body.split()).casefold(),
            FULL_COMMAND='@coderabbitai full review'),
        'github': SimpleNamespace(immutable_database_id=lambda row: row.get('databaseId'),
                                  is_coderabbit_login=lambda name: name in ('coderabbitai', 'coderabbitai[bot]')),
        'sqlite_hosted_capture': SimpleNamespace(_validate_trigger_record=lambda repo, pr, record: dict(record),
                                                 _complete_pull_request=complete, _terminal_event=terminal,
                                                 _findings_for_completed_result=findings),
    }


class ResultTests(unittest.TestCase):
    def helpers(self):
        return synthetic_helpers()

    def setUp(self):
        self.modules = self.helpers()
        self.payload, self.metadata, self.history = fixture()
        capture, hosted = self.modules['sqlite_hosted_capture'], self.modules['hosted']
        pull = capture._complete_pull_request(self.payload, 7)
        result = hosted.trigger_state(REPO, 7, self.payload, self.metadata['trigger_record'],
                                      current_record_path=None, now=datetime.fromisoformat(FINISH.replace('Z', '+00:00')))
        observed = capture._findings_for_completed_result(
            result, pull, self.metadata['trigger_record'], response_review=pull['reviews']['nodes'][0],
            response_comment=None, finished_at=FINISH)
        for index, item in enumerate(observed):
            disposition = ('unresolved', 'accepted', 'rejected', 'routed')[index]
            self.history['findings'].append(dict(run_id=RUN, finding_id='finding-' + str(index),
                source_pr=7, source_channel='hosted', source_finding_key=item.source_finding_key,
                title=item.title, detail=item.detail, disposition=disposition,
                route_id='route-3' if disposition == 'routed' else None))
        self.artifacts = {}
        self.refresh_archive()
        self.records = SimpleNamespace(attempt_artifacts=Mock(side_effect=lambda _: deepcopy(self.artifacts)))

    def refresh_archive(self):
        pull = self.payload['data']['repository']['pullRequest']
        self.artifacts.update(metadata=json.dumps(self.metadata),
                              hosted_review=json.dumps(pull['reviews']['nodes']),
                              hosted_comments=json.dumps({'comments': pull['comments']['nodes'],
                                                          'review_threads': pull['reviewThreads']['nodes']}))

    def validate(self):
        return validate_unsettled_hosted(self.modules, self.records, self.history, RUN, REPO, self.payload)

    def refusal(self):
        with self.assertRaises(ResultRefusal) as caught:
            self.validate()
        self.assertEqual(str(caught.exception), 'unsettled_hosted_result_invalid')
        self.assertIsNone(caught.exception.__cause__)

    def test_exact_proof_originals_and_replay_preserve_inputs_without_credit(self):
        before = deepcopy((self.payload, self.history, self.artifacts))
        value = self.validate()
        self.assertEqual(value, self.validate())
        self.assertEqual(before, (self.payload, self.history, self.artifacts))
        self.assertEqual(value['deferred'], [(7, 'hosted')])
        self.assertEqual([row['kind'] for row in value['obligations']], ['result', 'finding', 'finding'])
        result, *findings = value['obligations']
        self.assertEqual(result['source_key'], 'result:hosted:' + RUN)
        self.assertEqual(len(result['evidence']), 7)
        self.assertEqual(value['proof']['response_id'], 201)
        self.assertEqual(set(value['proof']['artifacts_sha256']), set(self.artifacts))
        self.assertEqual([row['source_key'] for row in findings],
                         ['finding:' + RUN + ':finding-0', 'finding:' + RUN + ':finding-1'])
        for original, retained in zip(findings, self.history['findings']):
            self.assertEqual(original['summary'], retained['title'] + '\n' + retained['detail'])
            self.assertEqual(original['original_status'], 'pending')
        self.assertNotIn('counts', value)
        self.assertNotIn('counts', value['proof'])

    def test_result_remains_without_finding_holds(self):
        for row in self.history['findings']:
            row.update(disposition='rejected', route_id=None)
        self.assertEqual([row['kind'] for row in self.validate()['obligations']], ['result'])

    def test_attempt_precedes_posted_trigger_and_preserves_both_source_clocks(self):
        earlier = '2026-01-02T03:03:54Z'
        self.history['attempts'][0]['started_at'] = earlier
        before = deepcopy(self.history)
        value = self.validate()
        self.assertEqual(value['proof']['attempt_started_at'], earlier)
        self.assertEqual(value['proof']['started_at'], START)
        self.assertEqual(self.history, before)

    def test_canonical_payload_without_repository_name_uses_explicit_collector_binding(self):
        value = self.validate()
        del self.payload['data']['repository']['nameWithOwner']
        self.assertEqual(value, self.validate())
        for name in (None, True, 17, {}, 'other/project'):
            with self.subTest(name=name):
                self.payload['data']['repository']['nameWithOwner'] = name
                self.refusal()

    def test_repository_pr_head_attempt_and_trigger_identity_mismatch_refuse(self):
        mutations = [
            lambda: self.history.update(pr=True),
            lambda: self.history['runs'][0].update(source_pr=True),
            lambda: self.history['runs'][0].update(source_head='private-invalid-head'),
            lambda: self.history['attempts'][0].update(repository='other/project'),
            lambda: self.history['attempts'][0].update(candidate_sha='b' * 40),
            lambda: self.history['attempts'][0].update(run_id='other-run'),
            lambda: self.history['attempts'][0].update(attempt_id='other-attempt'),
            lambda: self.history['attempts'][0].update(provider_review_id='0201'),
            lambda: self.metadata.update(repository='other/project'),
            lambda: self.metadata.update(pull_request=True),
            lambda: self.metadata.update(trigger_id=True),
            lambda: self.metadata.update(response_id=202),
            lambda: self.metadata['trigger_record'].update(sqlite_attempt_id='other-attempt'),
            lambda: self.metadata['trigger_record'].update(repository='other/project'),
            lambda: self.metadata['trigger_record'].update(pr_number=True),
            lambda: self.metadata['trigger_record']['anchor'].update(child_head='b' * 40),
            lambda: self.metadata['trigger_record']['trigger'].update(id=102),
        ]
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                self.setUp()
                mutate()
                self.refresh_archive()
                self.refusal()

    def test_duplicate_run_attempt_and_observations_refuse(self):
        for key in ('runs', 'attempts', 'findings'):
            with self.subTest(key=key):
                self.setUp()
                self.history[key].append(deepcopy(self.history[key][0]))
                self.refusal()

    def test_two_hundred_canonical_observations_admit_and_extra_refuses(self):
        pull = self.payload['data']['repository']['pullRequest']
        first = deepcopy(pull['reviewThreads']['nodes'][0])
        pull['reviewThreads']['nodes'] = []
        self.history['findings'] = []
        for index in range(200):
            thread = deepcopy(first)
            thread['id'] = f'owned-thread-{index}'
            thread['comments']['nodes'][0]['databaseId'] = 301 + index
            pull['reviewThreads']['nodes'].append(thread)
        capture, hosted = self.modules['sqlite_hosted_capture'], self.modules['hosted']
        state = hosted.trigger_state(REPO, 7, self.payload, self.metadata['trigger_record'])
        observations = capture._findings_for_completed_result(
            state, pull, self.metadata['trigger_record'], response_review=pull['reviews']['nodes'][0],
            response_comment=None, finished_at=FINISH)
        for index, row in enumerate(observations):
            self.history['findings'].append(dict(run_id=RUN, finding_id=f'finding-{index}',
                source_pr=7, source_channel='hosted', source_finding_key=row.source_finding_key,
                title=row.title, detail=row.detail, disposition='rejected', route_id=None))
        self.refresh_archive()
        self.assertEqual(len(self.validate()['obligations']), 1)
        extra = deepcopy(self.history['findings'][0])
        extra.update(finding_id='finding-201', source_finding_key='hosted-comment:999')
        self.history['findings'].append(extra)
        self.refusal()

    def test_terminal_flags_and_finalization_refuse(self):
        for owner, key, value in (
            ('run', 'outcome', 'started'), ('run', 'attributable', 1),
            ('run', 'finalized', True), ('run', 'finalized_at', FINISH),
            ('attempt', 'state', 'started'), ('meta', 'terminal', 1),
            ('meta', 'state', 'active'), ('meta', 'attributable', False),
        ):
            with self.subTest(owner=owner, key=key):
                self.setUp()
                target = {'run': self.history['runs'][0], 'attempt': self.history['attempts'][0],
                          'meta': self.metadata}[owner]
                target[key] = value
                self.refresh_archive()
                self.refusal()

    def test_malformed_times_and_clock_disagreement_refuse(self):
        for value in (None, True, '', '2026-01-02T03:04:05', 'private-bad-time',
                      '2026-01-02T03:04:04Z', '2026-01-02T03:05:06Z'):
            with self.subTest(value=value):
                self.setUp()
                self.metadata['observed_at'] = value
                self.refresh_archive()
                self.refusal()
        self.setUp()
        self.history['attempts'][0]['started_at'] = FINISH
        self.refusal()

    def test_current_payload_scope_completion_and_response_drift_refuse(self):
        mutations = [
            lambda p: p['data']['repository'].update(nameWithOwner='other/project'),
            lambda p: p['data']['repository']['pullRequest'].update(number=8),
            lambda p: p['data']['repository']['pullRequest'].update(headRefOid='b' * 40),
            lambda p: p['data']['repository']['pullRequest']['reviews'].update(pageInfo={'hasNextPage': True}),
            lambda p: p['data']['repository']['pullRequest']['reviews']['nodes'][0].update(databaseId=202),
            lambda p: p['data']['repository']['pullRequest']['reviews']['nodes'][0].update(commit={'oid': 'b' * 40}),
            lambda p: p['data']['repository']['pullRequest']['reviews']['nodes'][0].update(submittedAt=START),
            lambda p: p['data']['repository']['pullRequest']['reviews']['nodes'][0].update(author={'login': 'stranger'}),
            lambda p: p['data']['repository']['pullRequest']['reviews']['nodes'][0].update(state='DISMISSED'),
        ]
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                self.setUp()
                mutate(self.payload)
                self.refusal()

    def test_current_and_archived_findings_require_exact_keys_content_and_cardinality(self):
        for archive in (False, True):
            for mutation in ('body', 'id', 'missing', 'duplicate'):
                with self.subTest(archive=archive, mutation=mutation):
                    self.setUp()
                    if archive:
                        value = json.loads(self.artifacts['hosted_comments'])
                        threads = value['review_threads']
                    else:
                        threads = self.payload['data']['repository']['pullRequest']['reviewThreads']['nodes']
                    if mutation == 'body':
                        threads[0]['comments']['nodes'][0]['body'] = '**Changed private finding**'
                    elif mutation == 'id':
                        threads[0]['comments']['nodes'][0]['databaseId'] = 999
                    elif mutation == 'missing':
                        threads.pop()
                    else:
                        threads.append(deepcopy(threads[0]))
                    if archive:
                        self.artifacts['hosted_comments'] = json.dumps(value)
                    self.refusal()

    def test_rejected_and_routed_observations_are_not_omitted_from_content_proof(self):
        for index in (2, 3):
            with self.subTest(index=index):
                self.setUp()
                self.history['findings'][index]['detail'] += ' changed'
                self.refusal()

    def test_malformed_disposition_scope_and_native_original_limits_refuse(self):
        for change in (dict(disposition='unknown'), dict(route_id='unexpected'),
                       dict(source_channel='cli'), dict(source_pr=True),
                       dict(title='x' * (16 << 10)), dict(finding_id='x' * 65),
                       dict(source_finding_key='x' * 201), dict(title='private\x00text')):
            with self.subTest(change=change):
                self.setUp()
                self.history['findings'][0].update(change)
                self.refusal()
        self.setUp()
        self.history['findings'] *= 51
        self.refusal()

    def test_artifact_limits_duplicate_json_and_invalid_shapes_refuse(self):
        for kind, value in (('metadata', 'x' * ((1 << 20) + 1)),
                            ('metadata', '{"state":1,"state":2}'), ('metadata', 'NaN'),
                            ('hosted_review', '{}'), ('hosted_comments', '[]'),
                            ('metadata', b'private bytes'), ('metadata', '{private malformed')):
            with self.subTest(kind=kind, value_type=type(value)):
                self.setUp()
                self.artifacts[kind] = value
                self.refusal()
        for change in ('missing', 'extra'):
            self.setUp()
            if change == 'missing':
                del self.artifacts['metadata']
            else:
                self.artifacts['other'] = 'private extra'
            self.refusal()

    def test_archive_budget_boundary_is_measured_in_utf8_bytes(self):
        original = self.artifacts['metadata']
        # JSON permits trailing whitespace; a complete exactly-1-MiB artifact
        # retains the same meaning, while its raw byte digest remains distinct.
        self.artifacts['metadata'] = original + ' ' * ((1 << 20) - len(original.encode('utf-8')))
        boundary = self.validate()
        self.assertNotEqual(boundary['proof']['artifacts_sha256']['metadata'],
                            hashlib.sha256(original.encode()).hexdigest())
        self.artifacts['metadata'] += ' '
        self.refusal()
        self.artifacts['metadata'] = original + '\u00a0' * (1 << 19)
        self.refusal()

    def test_source_exception_text_never_escapes(self):
        self.records.attempt_artifacts.side_effect = ValueError('private path /secret and body token')
        self.refusal()

    def test_source_terminal_classifier_is_required_for_both_payloads(self):
        hosted = self.modules['hosted']
        classify = hosted.trigger_state
        for pass_index in (0, 1):
            for changed in (dict(state='active'), dict(terminal=False), dict(attributed=False),
                            dict(response_id=999), dict(repository='other/project')):
                with self.subTest(pass_index=pass_index, changed=changed):
                    count = 0

                    def altered(*args, **kwargs):
                        nonlocal count
                        original = classify(*args, **kwargs)
                        result = SimpleNamespace(**vars(original))
                        if count == pass_index:
                            result.__dict__.update(changed)
                        count += 1
                        return result

                    with patch.object(hosted, 'trigger_state', side_effect=altered):
                        self.refusal()
                    self.assertEqual(count, pass_index + 1)

    def test_review_backed_finished_reply_uses_canonical_classifier_without_discovery(self):
        comment = dict(databaseId=999, author={'login': 'coderabbitai[bot]'},
                       body='Full review finished.', createdAt=FINISH, updatedAt=FINISH)
        self.payload['data']['repository']['pullRequest']['comments']['nodes'].append(comment)
        self.refresh_archive()
        with ExitStack() as stack:
            for target in ('builtins.open', 'os.open', 'subprocess.run', 'subprocess.Popen'):
                stack.enter_context(patch(target, side_effect=AssertionError('forbidden side effect')))
            stack.enter_context(patch.object(socket, 'socket', side_effect=AssertionError('no network')))
            hosted = self.modules['hosted']
            for name in ('default_trigger_record_path', '_archived_completed_thread_bodies',
                         '_unresolved_retired_predecessor', 'SqliteReviewRecords'):
                if hasattr(hosted, name):
                    stack.enter_context(patch.object(hosted, name, side_effect=AssertionError('no discovery')))
            self.assertEqual(self.validate()['proof']['response_id'], 201)

    def test_later_trigger_at_or_before_expected_review_refuses_before_current_classification(self):
        for created in (START, '2026-01-02T03:04:30Z', FINISH):
            with self.subTest(created=created):
                self.setUp()
                self.payload['data']['repository']['pullRequest']['comments']['nodes'].append(
                    dict(databaseId=999, author={'login': 'owner'},
                         body='@coderabbitai full review', createdAt=created, updatedAt=created))
                with patch.object(self.modules['hosted'], 'trigger_state',
                                  wraps=self.modules['hosted'].trigger_state) as classify:
                    self.refusal()
                self.assertEqual(classify.call_count, 1)

    def test_no_filesystem_provider_sql_or_mutation_calls(self):
        with ExitStack() as stack:
            for target in ('builtins.open', 'os.open', 'subprocess.run', 'subprocess.Popen'):
                stack.enter_context(patch(target, side_effect=AssertionError('forbidden side effect')))
            stack.enter_context(patch.object(socket, 'socket', side_effect=AssertionError('no network')))
            hosted = self.modules['hosted']
            for name in ('default_trigger_record_path', 'SqliteReviewRecords', 'load_trigger_record'):
                if hasattr(hosted, name):
                    stack.enter_context(patch.object(hosted, name, side_effect=AssertionError('no discovery')))
            self.assertEqual(self.validate()['proof']['run_id'], RUN)
        self.records.attempt_artifacts.assert_called_once_with(RUN)


@unittest.skipUnless(os.environ.get('FIRECONTROLLER_SOURCE_ROOT'), 'explicit trusted source root not selected')
class TrustedResultTests(ResultTests):
    def helpers(self):
        source = Path(os.environ['FIRECONTROLLER_SOURCE_ROOT']) / 'dev-tools'
        sys.dont_write_bytecode = True
        sys.path.insert(0, str(source))
        modules = {name: importlib.import_module('pr_review.' + name)
                   for name in ('hosted', 'github', 'sqlite_hosted_capture')}
        for module in modules.values():
            self.assertTrue(Path(module.__file__).resolve().is_relative_to(source.resolve()))
        return modules


if __name__ == '__main__':
    unittest.main()
