# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Strict source-owner carried-frame admission; no collection or native I/O.

Bindings identify unchanged canonical history rows, not altered counting facts.
The trusted collector owns positive audit/record joins. This reader checks that
the owner's complete ledger, row bindings and deferred controls agree.
"""
import hashlib
import json
import re

from source_obligation_routes import _native_json, _pr, _source_key, _text


class CarriedFrameRefusal(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def canonical(value):
    return (json.dumps(value, sort_keys=True, ensure_ascii=True,
                       separators=(',', ':'), allow_nan=False) + '\n').encode()


def row_digest(row):
    return hashlib.sha256(canonical(row)).hexdigest()


def _require(condition):
    if not condition:
        raise CarriedFrameRefusal('carried_frame_invalid')


def _keys(value, fields):
    return type(value) is dict and set(value) == set(fields)


def _array(value, maximum, *, empty=True):
    return type(value) is list and (empty or bool(value)) and len(value) <= maximum


def _hash(value):
    return type(value) is str and re.fullmatch('[0-9a-f]{64}', value) is not None


def _validate(frame, repository_id, snapshot_sha256):
    """Return unchanged history rows and their exact carry/defer bindings.

    This validates an owner assertion, not the truth of a provider audit or a
    native receipt. Never use it instead of complete source collection.
    """
    _require(_keys(frame, ('version', 'repository_id', 'snapshot_sha256',
        'projection_complete', 'quiescent', 'obligations_clear', 'prs',
        'obligations', 'bindings', 'deferred_controls')))
    _require(type(frame['version']) is int and frame['version'] == 2
        and type(repository_id) is int and 0 < repository_id < 2**63
        and type(frame['repository_id']) is int and frame['repository_id'] == repository_id
        and _hash(snapshot_sha256) and frame['snapshot_sha256'] == snapshot_sha256
        and frame['projection_complete'] is True and frame['quiescent'] is True
        and frame['obligations_clear'] is False)
    _require(_array(frame['prs'], 100, empty=False)
        and _array(frame['obligations'], 1000, empty=False)
        and _array(frame['bindings'], 10000)
        and _array(frame['deferred_controls'], 200))
    _require(len(canonical(frame)) <= 1 << 20)
    originals = {}
    for original in frame['obligations']:
        _require(_keys(original, ('source_key', 'kind', 'affected_pull_numbers',
                                 'summary', 'evidence', 'original_status')))
        key, kind = original['source_key'], original['kind']
        _require(_source_key(key) and key not in originals
            and type(kind) is str and kind in ('finding', 'route', 'result')
            and original['original_status'] in ('pending', 'open')
            and _text(original['summary'], 16 << 10))
        numbers, evidence = original['affected_pull_numbers'], original['evidence']
        _require(_array(numbers, 100, empty=False) and all(_pr(n) for n in numbers)
            and numbers == sorted(set(numbers)) and _array(evidence, 10)
            and all(_text(ref, 1024) for ref in evidence)
            and len(_native_json(original)) <= 32 << 10)
        originals[key] = original
    histories, rows, seen_prs = {}, {}, set()
    forbidden = ('active_review', 'active_reservation', 'ambiguous',
                 'terminal_ambiguous', 'unstable', 'unreconciled',
                 'parent_moved', 'over_ceiling', 'rate_limited', 'actionable')
    booleans = forbidden + ('completed', 'attributable', 'anchored',
        'provisional', 'correction', 'non_counting', 'held', 'finding_only_hold')
    for pull in frame['prs']:
        _require(_keys(pull, ('pull_number', 'channels')) and _pr(pull['pull_number'])
            and type(pull['channels']) is dict and bool(pull['channels'])
            and not set(pull['channels']) - {'cli', 'hosted'})
        pr = pull['pull_number']
        _require(pr not in seen_prs)
        seen_prs.add(pr)
        for channel, history in pull['channels'].items():
            identity = (pr, channel)
            _require(identity not in histories and _array(history, 10000))
            histories[identity] = history
            for row in history:
                _require(type(row) is dict and type(row.get('pr')) is int and row['pr'] == pr
                    and row.get('channel', channel) == channel
                    and all(row.get(k) is None or type(row[k]) is bool for k in booleans)
                    and all(row.get(k) is not True for k in forbidden))
                if 'accepted' in row:
                    _require(type(row['accepted']) is int and 0 <= row['accepted'] < 2**63)
                digest = row_digest(row)
                rows.setdefault((pr, channel, digest), []).append(row)
    selected_prs = {pr for pr, _ in histories}
    _require(all(set(original['affected_pull_numbers']) <= selected_prs
                 for original in originals.values()))
    bindings = {}
    for binding in frame['bindings']:
        _require(_keys(binding, ('pull_number', 'channel', 'row_sha256', 'source_keys')))
        pr, channel, sha = (binding[k] for k in ('pull_number', 'channel', 'row_sha256'))
        _require(_pr(pr) and type(channel) is str and channel in ('cli', 'hosted') and _hash(sha))
        identity = (pr, channel, sha)
        keys = binding['source_keys']
        _require(identity not in bindings and len(rows.get(identity, [])) == 1
            and _array(keys, 1000, empty=False) and all(_source_key(key) for key in keys)
            and keys == sorted(set(keys))
            and all(key in originals and pr in originals[key]['affected_pull_numbers'] for key in keys))
        bindings[identity] = keys
    deferred = {}
    for entry in frame['deferred_controls']:
        _require(_keys(entry, ('pull_number', 'channel', 'result_source_keys')))
        pr, channel, keys = entry['pull_number'], entry['channel'], entry['result_source_keys']
        _require(_pr(pr) and type(channel) is str and channel in ('cli', 'hosted'))
        identity = (pr, channel)
        _require(identity in histories and identity not in deferred
            and _array(keys, 1000, empty=False) and all(_source_key(key) for key in keys)
            and keys == sorted(set(keys))
            and all(key in originals and originals[key]['kind'] == 'result'
                    and pr in originals[key]['affected_pull_numbers'] for key in keys))
        deferred[identity] = keys
    retained_results = {key for keys in deferred.values() for key in keys}
    _require(all(key in retained_results for key, original in originals.items()
                 if original['kind'] == 'result'))
    for (pr, channel), history in histories.items():
        for row in history:
            keys = bindings.get((pr, channel, row_digest(row)), [])
            kinds = {originals[key]['kind'] for key in keys}
            if row.get('finding_only_hold') is True:
                _require(row.get('held') is True and 'finding' in kinds)
            if row.get('held') is True:
                if row.get('finding_only_hold') is True:
                    _require('finding' in kinds)
                else:
                    _require('result' in kinds and (pr, channel) in deferred
                        and set(keys) <= set(deferred[(pr, channel)]))
            status = row.get('source_resolution_status')
            if status is not None and status != 'resolved':
                _require(status == 'finding_pending' and 'finding' in kinds)
            eligible_accepted = (row.get('completed') is True and row.get('attributable') is True
                and not any(row.get(k) for k in ('provisional', 'correction', 'non_counting'))
                and row.get('accepted', 0) > 0)
            if eligible_accepted:
                _require(status == 'resolved' or status == 'finding_pending' and 'finding' in kinds)
    return dict(histories=histories, originals=originals, bindings=bindings, deferred=deferred)


def validate(frame, repository_id, snapshot_sha256):
    try:
        return _validate(frame, repository_id, snapshot_sha256)
    except CarriedFrameRefusal:
        raise
    except Exception:
        raise CarriedFrameRefusal('carried_frame_invalid') from None


def selected_history(frame, selected, snapshot_sha256):
    _require(type(selected) is dict and type(selected.get('repository_id')) is int
        and 0 < selected['repository_id'] < 2**63 and _pr(selected.get('pull_number'))
        and type(selected.get('channel')) is str and selected['channel'] in ('cli', 'hosted'))
    value = validate(frame, selected['repository_id'], snapshot_sha256)
    identity = (selected['pull_number'], selected['channel'])
    _require(identity in value['histories'])
    if identity in value['deferred']:
        raise CarriedFrameRefusal('controls_deferred')
    return value['histories'][identity]
