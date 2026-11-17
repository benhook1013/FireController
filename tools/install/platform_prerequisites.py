"""Read-only observations of explicitly selected deployment executable pins.

Does not invoke programs, search PATH, or establish runtime behavior, permissions
inside service namespaces, credentials, or operation admission authority.
"""
import hashlib
import os
from pathlib import PurePosixPath
import re
import stat
from lifecycle_package import Invalid, identity, open_directory

ROLES = frozenset(('python-interpreter', 'kubernetes-cli', 'containerd-cli', 'systemd-cli'))
MAX_EXECUTABLE = 256 << 20


def executable_pin(path, expected_sha256):
    if not isinstance(expected_sha256, str) or not re.fullmatch('[0-9a-f]{64}', expected_sha256):
        raise Invalid('independently selected executable digest required')
    if not isinstance(path, str):
        raise Invalid('explicit executable path required')
    parsed = PurePosixPath(path)
    if not parsed.is_absolute() or str(parsed) != path or '..' in parsed.parts or parsed.name in ('', '/'):
        raise Invalid('canonical executable path required')
    parent = open_directory(str(parsed.parent))
    source = None
    try:
        source = os.open(parsed.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        before = os.fstat(source)
        if (not stat.S_ISREG(before.st_mode) or not before.st_mode & 0o111
                or before.st_mode & 0o022 or not 0 < before.st_size <= MAX_EXECUTABLE):
            raise Invalid('bounded non-writable regular executable required')
        digest = hashlib.sha256()
        total = 0
        while total <= MAX_EXECUTABLE:
            block = os.read(source, min(65536, MAX_EXECUTABLE + 1 - total))
            if not block:
                break
            total += len(block)
            digest.update(block)
        if total > MAX_EXECUTABLE or total != before.st_size or digest.hexdigest() != expected_sha256:
            raise Invalid('executable digest or size mismatch')
        if (identity(os.fstat(source)) != identity(before)
                or identity(os.stat(parsed.name, dir_fd=parent, follow_symlinks=False)) != identity(before)):
            raise Invalid('executable changed during observation')
        # Check the named parent still resolves to the held directory; a future
        # replacement after observation is not fenced and cannot grant admission.
        current_parent = open_directory(str(parsed.parent))
        try:
            if identity(os.fstat(current_parent)) != identity(os.fstat(parent)):
                raise Invalid('executable parent changed')
        finally:
            os.close(current_parent)
        return {'sha256': expected_sha256, 'size': total, 'bytes_verified': True,
                'execution_verified': False}
    except OSError:
        raise Invalid('executable unavailable or unsupported') from None
    finally:
        if source is not None:
            os.close(source)
        os.close(parent)


def inspect(declarations):
    if not isinstance(declarations, dict) or set(declarations) != ROLES:
        raise Invalid('all explicit platform roles required')
    observations = {}
    for role, value in sorted(declarations.items()):
        if not isinstance(value, dict) or set(value) != {'path', 'sha256'}:
            raise Invalid('explicit path and independent digest required')
        observations[role] = executable_pin(value['path'], value['sha256'])
    return {'scope': 'selected-host-executable-bytes-only', 'roles': observations,
            'provider_calls': 0, 'executables_run': 0,
            'platform_runtime_verified': False, 'generation_switch_ready': False,
            'installation_ready': False,
            'unresolved': ['interpreter standard library and executable shared libraries',
                           'service namespace and effective command/environment bindings',
                           'configured Kubernetes/containerd/systemd endpoints and authority',
                           'fresh admission custody and coordinated generation recovery']}
