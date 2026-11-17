"""Verify rendered release bytes and their source/binary/image binding.
This verifies receipt consistency, not receipt authority, runtime configuration,
service quiescence, rollback compatibility, or permission to switch generations.
"""
import hashlib
import json
import os
import re
from pathlib import PurePosixPath
from lifecycle_package import Invalid, open_directory, read_file, identity


def decode(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise Invalid('duplicate receipt key')
            result[key] = value
        return result
    value = json.loads(raw, object_pairs_hook=unique)
    if not isinstance(value, dict):
        raise Invalid('receipt object required')
    return value


def member(root_fd, name, limit):
    if not isinstance(name, str):
        raise Invalid('relative operation member required')
    p = PurePosixPath(name)
    if p.is_absolute() or str(p) != name or not p.parts or '..' in p.parts:
        raise Invalid('canonical relative operation member required')
    fd = os.dup(root_fd)
    try:
        for part in p.parts[:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        return read_file(fd, p.name, limit)
    finally:
        os.close(fd)


def inspect(root, expected_manifest_sha256, expected_source, expected_image):
    if not re.fullmatch(r'[0-9a-f]{64}', expected_manifest_sha256):
        raise Invalid('independently pinned manifest required')
    if not re.fullmatch(r'[0-9a-f]{40}', expected_source):
        raise Invalid('exact source required')
    if not isinstance(expected_image, str) or not re.fullmatch(r'[^\s@]+@sha256:[0-9a-f]{64}', expected_image):
        raise Invalid('exact digest image required')
    fd = open_directory(root)
    try:
        root_identity = identity(os.fstat(fd))
        raw, manifest_identity = member(fd, 'execution-hashes.json', 1 << 20)
        if hashlib.sha256(raw).hexdigest() != expected_manifest_sha256:
            raise Invalid('execution manifest changed')
        hashes = decode(raw)
        required = {'canonical-proof.json', 'release/bin/fcd'}
        if not required <= hashes.keys() or not 2 <= len(hashes) <= 128:
            raise Invalid('bounded execution manifest missing source proof or executable')
        receipts = {}
        identities = {'execution-hashes.json': manifest_identity}
        total = 0
        for name, digest in sorted(hashes.items()):
            if name == 'execution-hashes.json' or not isinstance(digest, str) or not re.fullmatch(r'[0-9a-f]{64}', digest):
                raise Invalid('invalid member digest')
            data, observed = member(fd, name, 128 << 20)
            total += len(data)
            if total > 256 << 20 or hashlib.sha256(data).hexdigest() != digest:
                raise Invalid('operation member mismatch or aggregate budget exceeded')
            identities[name] = observed
            if name == 'canonical-proof.json':
                receipts[name] = decode(data)
        # Image receipt is produced later than the pre-execution hash manifest.
        # Bind its claims to independently selected source/image and hashed binary.
        image_raw, image_identity = member(fd, 'image-manifest.json', 1 << 20)
        identities['image-manifest.json'] = image_identity
        proof, image = receipts['canonical-proof.json'], decode(image_raw)
        binary = hashes['release/bin/fcd']
        if proof.get('source') != expected_source or image.get('source') != expected_source:
            raise Invalid('source receipt mismatch')
        if proof.get('binary_sha256') != binary or image.get('binary_sha256') != binary:
            raise Invalid('binary receipt mismatch')
        if image.get('image') != expected_image:
            raise Invalid('image receipt mismatch')
        # Reopen through the root again so replaced intermediate directories fail.
        for name, before in identities.items():
            _, after = member(fd, name, 128 << 20)
            if before != after:
                raise Invalid('operation member replaced during inspection')
        current = open_directory(root)
        try:
            if identity(os.fstat(current)) != root_identity:
                raise Invalid('operation root replaced')
        finally:
            os.close(current)
        return {'source': expected_source, 'image': expected_image,
                'binary_sha256': binary, 'verified_members': len(hashes),
                'execution_manifest_sha256': expected_manifest_sha256,
                'receipt_bytes_consistent': True, 'dependency_closure_verified': False,
                'installation_ready': False}
    finally:
        os.close(fd)
