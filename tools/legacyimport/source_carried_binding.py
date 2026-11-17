# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Offline binding of a carried ledger to retained native preview receipts.

The operator obtains these receipts from actual native read-only previews after
applying the complete ledger. An offline file is not an authenticated server
attestation. Native policy preview/apply independently checks the current source
identity and revision at the selected destination.
"""
import hashlib
import re

from source_carried_frame import (validate, canonical, CarriedFrameRefusal,
                                  _keys, _array, _hash, _require)
from source_obligation_routes import _native_json, _source_key


def _reference(frame, selected, snapshot_sha256, proof):
    value = validate(frame, selected['repository_id'], snapshot_sha256)
    _require(_keys(proof, ('version', 'batches')) and type(proof['version']) is int
        and proof['version'] == 1 and _array(proof['batches'], 100, empty=False))
    found, project, revision = {}, None, None
    namespace = selected['source']
    _require(_source_key(namespace))
    for item in proof['batches']:
        _require(_keys(item, ('batch', 'preview')))
        batch, preview = item['batch'], item['preview']
        _require(_keys(batch, ('version', 'source', 'expected_revision', 'obligations'))
            and type(batch['version']) is int and batch['version'] == 1
            and _array(batch['obligations'], 100, empty=False))
        source = batch['source']
        _require(_keys(source, ('namespace', 'repository_id', 'snapshot_sha256'))
            and source['namespace'] == namespace
            and type(source['repository_id']) is int and source['repository_id'] == selected['repository_id']
            and source['snapshot_sha256'] == snapshot_sha256)
        epoch = batch['expected_revision']
        _require(type(epoch) is int and 1 <= epoch <= 2**53 - 1)
        _require(_keys(preview, ('project_id', 'source_revision', 'selected', 'new',
                                'unchanged', 'payload_sha256', 'preview_sha256')))
        _require(type(preview['project_id']) is str
            and re.fullmatch('[A-Za-z0-9_-]{1,128}', preview['project_id']) is not None
            and type(preview['source_revision']) is int and preview['source_revision'] == epoch
            and type(preview['selected']) is int and preview['selected'] == len(batch['obligations'])
            and type(preview['new']) is int and preview['new'] == 0
            and type(preview['unchanged']) is int and preview['unchanged'] == len(batch['obligations'])
            and _hash(preview['payload_sha256']) and _hash(preview['preview_sha256']))
        if project is None:
            project, revision = preview['project_id'], epoch
        _require(project == preview['project_id'] and revision == epoch)
        normalized = []
        for original in batch['obligations']:
            _require(type(original) is dict and _source_key(original.get('source_key')))
            key = original['source_key']
            _require(key not in found and key in value['originals']
                and canonical(original) == canonical(value['originals'][key]))
            found[key] = original
            # Native struct field order and HTML escaping are part of its
            # normalized payload digest. Frame JSON order is independent.
            normalized.append({name: original[name] for name in (
                'source_key', 'kind', 'affected_pull_numbers', 'summary',
                'evidence', 'original_status')})
        normalized.sort(key=lambda original: original['source_key'])
        native_batch = dict(version=1, source=dict(namespace=namespace,
            repository_id=selected['repository_id'], snapshot_sha256=snapshot_sha256),
            expected_revision=epoch, obligations=normalized)
        body = _native_json(native_batch)
        _require(len(_native_json(dict(batch=native_batch, preview_sha256='0'*64))) <= 256 << 10
            and hashlib.sha256(body).hexdigest() == preview['payload_sha256'])
    _require(set(found) == set(value['originals']))
    return dict(namespace=namespace, snapshot_sha256=snapshot_sha256, revision=revision)


def reference(frame, selected, snapshot_sha256, proof):
    try:
        return _reference(frame, selected, snapshot_sha256, proof)
    except CarriedFrameRefusal:
        raise
    except Exception:
        raise CarriedFrameRefusal('carried_frame_invalid') from None
