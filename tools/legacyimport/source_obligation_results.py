# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Offline proof of one retained, unsettled, review-backed Hosted result.

This is not a stop audit, clearance assertion, control exporter or counter
projection. The caller supplies trusted source modules, a canonical history
ledger, its read-only records reader and the collector's complete current
payload. The caller must bind that collection to the explicit repository
(including its numeric identity); the canonical source query need not include
nameWithOwner. A present repository name must match. An archived request
window alone never proves current completeness.
Only review-backed results are supported. A positively bound review in the
trigger's open window keeps the source's finished-reply fallback from consulting
ambient archived state; the canonical classifier still sees the whole payload.
"""
from collections.abc import Mapping
from datetime import datetime
import hashlib
import json
import re
import unicodedata


class ResultRefusal(Exception):
    """Fixed public diagnostic; never include source text or helper exceptions."""

    code = "unsettled_hosted_result_invalid"

    def __init__(self):
        super().__init__(self.code)


def _require(condition):
    if not condition:
        raise ResultRefusal()


def _text(value, maximum, *, empty=False, identifier=False):
    _require(isinstance(value, str) and (empty or bool(value.strip())))
    _require(len(value.encode('utf-8')) <= maximum)
    _require(not any(unicodedata.category(c) == 'Cc'
                     and (identifier or c not in '\t\n\r') for c in value))
    if identifier:
        _require(value == value.strip())
    return value


def _time(value):
    _text(value, 100)
    parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
    _require(parsed.tzinfo is not None and parsed.utcoffset() is not None)
    return parsed


def _positive(value):
    _require(type(value) is int and 0 < value <= 2**63 - 1)
    return value


def _json_object(pairs):
    result = {}
    for key, value in pairs:
        _require(key not in result)
        result[key] = value
    return result


def _original(key, kind, pr, summary, evidence):
    _text(key, 200, identifier=True)
    _text(summary, 16 << 10)
    _require(len(evidence) <= 10)
    for ref in evidence:
        _text(ref, 1024)
    value = dict(source_key=key, kind=kind, affected_pull_numbers=[pr],
                 summary=summary, evidence=evidence, original_status='pending')
    # Go's canonical JSON escapes HTML and the two JavaScript separators.
    encoded = json.dumps(value, ensure_ascii=False, separators=(',', ':'))
    for char, escaped in (('&', '\\u0026'), ('<', '\\u003c'), ('>', '\\u003e'),
                          ('\u2028', '\\u2028'), ('\u2029', '\\u2029')):
        encoded = encoded.replace(char, escaped)
    _require(len(encoded.encode('utf-8')) <= 32 << 10)
    return value


def _observations(rows):
    _require(isinstance(rows, (list, tuple)) and len(rows) <= 200)
    result = set()
    for row in rows:
        key, title, detail = row.source_finding_key, row.title, row.detail
        _text(key, 200, identifier=True)
        _text(title, 16 << 10)
        _text(detail, 16 << 10, empty=True)
        _require(not any(item[0] == key for item in result))
        result.add((key, title, detail))
    return result


def _validate(helpers, records, history, run_id, repository, current_payload):
    _text(run_id, 100, identifier=True)
    _require(isinstance(repository, str)
             and re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository))
    hosted, github, capture = (helpers[name] for name in
                              ('hosted', 'github', 'sqlite_hosted_capture'))
    _require(isinstance(history, Mapping))
    for name in ('runs', 'attempts', 'findings'):
        _require(type(history.get(name)) is list
                 and all(isinstance(row, Mapping) for row in history[name]))
    runs = [row for row in history['runs'] if row.get('run_id') == run_id]
    _require(len(runs) == 1)
    run = runs[0]
    pr, head = run.get('source_pr'), run.get('source_head')
    _require(type(pr) is int and 1 <= pr <= 2147483647
             and type(history.get('pr')) is int and history['pr'] == pr)
    _require(isinstance(head, str) and re.fullmatch('[0-9a-fA-F]{40}', head))
    _require(run.get('channel') == 'hosted' and run.get('outcome') == 'completed'
             and run.get('attributable') is True and run.get('finalized') is False
             and 'finalized_at' in run and run['finalized_at'] is None)
    started, finished = _time(run.get('started_at')), _time(run.get('finished_at'))
    _require(started <= finished)
    attempts = [row for row in history['attempts']
                if row.get('run_id') == run_id or row.get('attempt_id') == run_id]
    _require(len(attempts) == 1)
    attempt = attempts[0]
    _require(attempt.get('attempt_id') == run_id and attempt.get('run_id') == run_id
             and attempt.get('channel') == 'hosted' and attempt.get('state') == 'completed'
             and attempt.get('repository') == repository and attempt.get('candidate_sha') == head
             # The attempt starts before the trigger is posted; the source run
             # starts at that trigger. Preserve both clocks, with their order.
             and _time(attempt.get('started_at')) <= started
             and _time(attempt.get('finished_at')) == finished)
    for name in ('trigger_id', 'provider_review_id'):
        value = attempt.get(name)
        _require(isinstance(value, str) and re.fullmatch('[1-9][0-9]{0,18}', value))
        _positive(int(value))
    trigger_id, response_id = int(attempt['trigger_id']), int(attempt['provider_review_id'])
    artifacts = records.attempt_artifacts(run_id)
    kinds = ('metadata', 'hosted_review', 'hosted_comments')
    _require(isinstance(artifacts, Mapping) and set(artifacts) == set(kinds))
    parsed, hashes = {}, {}
    for kind in kinds:
        content = artifacts[kind]
        _require(isinstance(content, str))
        raw = content.encode('utf-8')
        _require(len(raw) <= 1 << 20)
        parsed[kind] = json.loads(content, object_pairs_hook=_json_object,
                                  parse_constant=lambda _: (_ for _ in ()).throw(ResultRefusal()))
        hashes[kind] = hashlib.sha256(raw).hexdigest()
    metadata, reviews, comments = (parsed[kind] for kind in kinds)
    _require(isinstance(metadata, dict) and isinstance(reviews, list)
             and isinstance(comments, dict) and type(comments.get('comments')) is list
             and type(comments.get('review_threads')) is list)
    _require(metadata.get('state') == 'completed' and metadata.get('terminal') is True
             and metadata.get('attributable') is True and metadata.get('repository') == repository
             and type(metadata.get('pull_request')) is int and metadata['pull_request'] == pr
             and metadata.get('head_sha') == head
             and _positive(metadata.get('trigger_id')) == trigger_id
             and _positive(metadata.get('response_id')) == response_id
             and _time(metadata.get('observed_at')) == finished)
    record = capture._validate_trigger_record(repository, pr, metadata.get('trigger_record'))
    _require(record.get('repository') == repository
             and type(record.get('pr_number')) is int and record['pr_number'] == pr
             and record.get('sqlite_attempt_id') == run_id and record.get('head_sha') == head
             and isinstance(record.get('anchor'), Mapping)
             and record['anchor'].get('child_head') == head
             and _positive(record['trigger']['id']) == trigger_id
             and _time(record['trigger']['created_at']) == started)
    archived = {'data': {'repository': {'nameWithOwner': repository, 'pullRequest': {
        'number': pr, 'headRefOid': head, 'reviews': {'nodes': reviews},
        'comments': {'nodes': comments['comments']},
        'reviewThreads': {'nodes': comments['review_threads']}}}}}
    expected = set()
    originals, finding_ids, keys = [], set(), set()
    retained = [row for row in history['findings'] if row.get('run_id') == run_id]
    _require(len(retained) <= 200)
    for row in retained:
        fid, key = row.get('finding_id'), row.get('source_finding_key')
        _text(fid, 64, identifier=True)
        _text(key, 200, identifier=True)
        title, detail = row.get('title'), row.get('detail')
        _text(title, 16 << 10)
        _text(detail, 16 << 10, empty=True)
        _require(fid not in finding_ids and key not in keys
                 and type(row.get('source_pr')) is int and row['source_pr'] == pr
                 and row.get('source_channel') == 'hosted'
                 and row.get('disposition') in ('unresolved', 'accepted', 'rejected', 'routed'))
        finding_ids.add(fid)
        keys.add(key)
        if row['disposition'] == 'routed':
            _text(row.get('route_id'), 200, identifier=True)
        else:
            _require(row.get('route_id') is None)
        expected.add((key, title, detail))
        if row['disposition'] in ('unresolved', 'accepted'):
            originals.append(_original('finding:' + run_id + ':' + fid, 'finding', pr,
                                       title + '\n' + detail,
                                       ['source-finding:' + fid, 'source-key:' + key,
                                        'source-run:' + run_id]))
    for payload in (archived, current_payload):
        _require(isinstance(payload, dict))
        repo = payload['data']['repository']
        _require('nameWithOwner' not in repo or repo['nameWithOwner'] == repository)
        pull = capture._complete_pull_request(payload, pr)
        _require(pull.get('headRefOid') == head)
        # The owning finished_reply_without_findings returns False upon a bot
        # review inside (trigger,next_trigger), before archived-body recovery.
        # Prove that early-return condition without deleting any reply or
        # changing source classification. A later/equal trigger cannot qualify.
        matching_reviews = [item for item in pull['reviews']['nodes']
                            if github.immutable_database_id(item) == response_id]
        _require(len(matching_reviews) == 1)
        expected_review = matching_reviews[0]
        _require(github.is_coderabbit_login((expected_review.get('author') or {}).get('login')))
        review_at = hosted.parse_timestamp(expected_review.get('submittedAt'))
        trigger_at = hosted.parse_timestamp(record['trigger']['created_at'])
        _require(review_at is not None and trigger_at is not None
                 and trigger_at < review_at <= finished)
        next_triggers = [when for item in pull['comments']['nodes']
                         if not github.is_coderabbit_login((item.get('author') or {}).get('login'))
                         and hosted.normalize_command(item.get('body') or '') == hosted.FULL_COMMAND
                         and (when := hosted.parse_timestamp(item.get('createdAt'))) is not None
                         and when >= trigger_at
                         and github.immutable_database_id(item) != trigger_id]
        _require(not next_triggers or review_at < min(next_triggers))
        result = hosted.trigger_state(repository, pr, payload, record, current_record_path=None,
                                      now=finished)
        _require(result.state == 'completed' and result.terminal is True and result.attributed is True
                 and type(result.response_id) is int and result.response_id == response_id
                 and result.repository == repository and type(result.pr_number) is int
                 and result.pr_number == pr and result.head_sha == head
                 and result.current_head_sha == head and result.trigger_comment_id == trigger_id
                 and _time(result.trigger_created_at) == started)
        event_at, review, comment = capture._terminal_event(result, pull, observed_at=run['finished_at'])
        _require(review is not None and comment is None
                 and github.immutable_database_id(review) == response_id
                 and isinstance(review.get('commit'), Mapping) and review['commit'].get('oid') == head
                 and github.is_coderabbit_login((review.get('author') or {}).get('login'))
                 and review.get('state') in ('COMMENTED', 'APPROVED', 'CHANGES_REQUESTED')
                 and _time(review.get('submittedAt')) > started
                 and _time(event_at) == finished)
        observed = capture._findings_for_completed_result(
            result, pull, record, response_review=review, response_comment=comment,
            finished_at=event_at)
        _require(_observations(observed) == expected)
    proof = dict(repository=repository, pull_request=pr, head_sha=head, run_id=run_id,
                 attempt_id=run_id, trigger_id=trigger_id, response_id=response_id,
                 started_at=run['started_at'], attempt_started_at=attempt['started_at'],
                 finished_at=run['finished_at'], artifacts_sha256=hashes)
    evidence = ['source-run:' + run_id, 'source-attempt:' + run_id,
                'source-trigger:' + str(trigger_id), 'source-response:' + str(response_id)]
    evidence.extend('source-artifact:' + kind + ':sha256:' + hashes[kind] for kind in kinds)
    result_original = _original('result:hosted:' + run_id, 'result', pr,
                                'Terminal source Hosted result awaits finalized counting and policy accounting.',
                                evidence)
    return dict(obligations=[result_original] + sorted(originals, key=lambda row: row['source_key']),
                deferred=[(pr, 'hosted')], proof=proof)


def validate_unsettled_hosted(helpers, records, history, run_id, repository, current_payload):
    """Return native originals, deferred channel and immutable proof, or refuse.

    Only ``records.attempt_artifacts`` and explicitly supplied pure source
    functions are called. Inputs and policy rows are never modified. The caller
    still owns read-only reader admission, repository-bound complete collection
    (including verified numeric repository identity) and global audit.
    """
    try:
        return _validate(helpers, records, history, run_id, repository, current_payload)
    except ResultRefusal:
        raise
    except Exception:
        raise ResultRefusal() from None
