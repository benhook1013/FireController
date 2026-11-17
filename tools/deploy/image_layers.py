"""Replace only verified terminal single-binary development overlays.

This changes newly built manifests; it never removes containerd content or tags.
"""
import copy
import gzip
import hashlib
import io
import tarfile

PREFIX = 'Install qualified clean develop FireController '


def replacement_base(manifest, config, get_blob):
    manifest, config = copy.deepcopy(manifest), copy.deepcopy(config)
    layers = manifest['layers']
    diffs = config['rootfs']['diff_ids']
    history = config.get('history', [])
    if len(layers) != len(diffs):
        raise ValueError('layer/diff identity mismatch')
    removed = 0
    while layers and history:
        entry = history[-1]
        if entry.get('empty_layer') or not entry.get('created_by', '').startswith(PREFIX):
            break
        layer = layers[-1]
        if layer.get('mediaType') != 'application/vnd.oci.image.layer.v1.tar+gzip':
            raise ValueError('unexpected daemon overlay format')
        compressed = get_blob(layer['digest'])
        if len(compressed) != layer['size'] or 'sha256:' + hashlib.sha256(compressed).hexdigest() != layer['digest']:
            raise ValueError('daemon overlay content identity mismatch')
        plain = gzip.decompress(compressed)
        if 'sha256:' + hashlib.sha256(plain).hexdigest() != diffs[-1]:
            raise ValueError('daemon overlay diff identity mismatch')
        with tarfile.open(fileobj=io.BytesIO(plain), mode='r:') as archive:
            entries = archive.getmembers()
            if len(entries) != 1 or entries[0].name != 'fcd' or not entries[0].isreg() or entries[0].mode != 0o555:
                raise ValueError('daemon overlay is not exclusively the replaced binary')
        layers.pop()
        diffs.pop()
        history.pop()
        removed += 1
    return manifest, config, removed
