"""Revalidate the exact prepared OCI graph before deferred admission.

Read-only; get_blob is the operation's bounded containerd content reader.
This verifies stored bytes, not live Kubernetes runtime or image authenticity.
"""
import hashlib
import json
import re

DIGEST = re.compile(r'sha256:[0-9a-f]{64}')


def checked_blob(get_blob, digest, size=None):
    if not isinstance(digest, str) or not DIGEST.fullmatch(digest):
        raise ValueError('invalid content identity')
    if size is not None and (type(size) is not int or size < 1 or size > 512*1024*1024):
        raise ValueError('invalid content size')
    raw = get_blob(digest)
    if not isinstance(raw, bytes) or len(raw) > 512*1024*1024:
        raise ValueError('invalid content bytes')
    if size is not None and len(raw) != size:
        raise ValueError('content size changed')
    if 'sha256:'+hashlib.sha256(raw).hexdigest() != digest:
        raise ValueError('content digest changed')
    return raw


def verify_content(report, source, binary_hash, previous_image, archive_hash, get_blob):
    if report['source'] != source or report['binary_sha256'] != binary_hash or report['previous_image'] != previous_image:
        raise ValueError('prepared source/preimage changed')
    if report['archive_sha256'] != archive_hash or report['unchanged_runtime_config'] is not True or report['source_revision_label_only'] is not True or report['added_layer_files'] != ['fcd']:
        raise ValueError('unqualified image producer result')
    image=report['image']
    if not isinstance(image,str) or not image.startswith('localhost/firecontroller@'):
        raise ValueError('unexpected image reference')
    manifest=json.loads(checked_blob(get_blob,image.split('@',1)[1]))
    layers=manifest['layers']
    if manifest['schemaVersion'] != 2 or not isinstance(layers,list) or not 1 <= len(layers) <= 128:
        raise ValueError('unbounded/invalid OCI graph')
    cfg=manifest['config']
    config=json.loads(checked_blob(get_blob,cfg['digest'],cfg['size']))
    if config['config']['Labels']['org.opencontainers.image.revision'] != source:
        raise ValueError('image revision changed')
    for layer in layers:
        checked_blob(get_blob,layer['digest'],layer['size'])
    return image
