#!/usr/bin/env python3
"""Render a qualified successor, with an explicit bounded-rehearsal decision."""
import argparse
import hashlib
import json
import os
from pathlib import Path
from prepare_rehearsal import render


def prepare(base, operation_dir, replacements, rehearsal):
    base = Path(base).resolve()
    manifest = json.loads((base/'manifest.json').read_text())
    files = {name:(base/name).read_bytes() for name in manifest['sha256']}
    for name, body in files.items():
        if hashlib.sha256(body).hexdigest() != manifest['sha256'][name]:
            raise ValueError('template changed: '+name)
    r = Path(operation_dir)
    if not r.is_absolute() or r.resolve() != r or not r.is_dir():
        raise ValueError('existing canonical operation directory required')
    if not isinstance(replacements, dict) or set(replacements) != {'image.py','rollout.py'}:
        raise ValueError('explicit replacements for both scripts required')
    outputs = {'image_layers.py':files['image_layers.py'],
               'rehearse.py':render(rehearsal, manifest['rehearsal_policy'], base).encode()}
    for target, source in [('image.py','image-next-template.py'), ('rollout.py','rollout-next-template.py')]:
        text = files[source].decode('utf-8-sig')
        mapping = replacements[target]
        if not isinstance(mapping, dict) or not mapping:
            raise ValueError('nonempty replacement map required')
        for old, new in mapping.items():
            if not isinstance(old, str) or not old or not isinstance(new, str) or old not in text:
                raise ValueError('replacement does not match template')
            text = text.replace(old,new)
        if manifest['original_operation'] in text or str(r) not in text:
            raise ValueError('operation path replacement incomplete')
        outputs[target] = text.encode()
    for name, body in outputs.items():
        compile(body, name, 'exec')
        if (r/name).exists() or (r/name).is_symlink():
            raise ValueError('output exists: '+name)
    for name, body in outputs.items():
        fd = os.open(r/name, os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'wb') as out:
            out.write(body)
            out.flush()
            os.fsync(out.fileno())
    return {'state':'prepared_only', 'template_source':manifest['source'],
            'rehearsal_mode':rehearsal['mode'],
            'outputs':{name:hashlib.sha256(body).hexdigest() for name,body in outputs.items()}}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for name in ('operation-dir','replacements','rehearsal'):
        p.add_argument('--'+name, required=True, type=Path)
    a=p.parse_args()
    print(json.dumps(prepare(Path(__file__).resolve().parent, a.operation_dir,
                            json.loads(a.replacements.read_text()),
                            json.loads(a.rehearsal.read_text())), sort_keys=True))

if __name__ == '__main__':
    main()
