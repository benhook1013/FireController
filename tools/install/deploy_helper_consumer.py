"""Read-only generation input validation. Never extracts or executes a package."""
import hashlib
import io
import json
import re
import tarfile
from deploy_helper_package import Invalid, LIMIT, MODULES, inspect_sources, module_inventory

MAX_ARCHIVE = (len(MODULES) + 1) * LIMIT
FIELDS = {'schema', 'source', 'scope', 'sha256', 'local_imports',
          'python_version_used_for_static_inventory', 'unresolved',
          'rendered_operation_verified', 'closure_verified', 'installation_ready'}


def pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise Invalid('duplicate package manifest field')
        result[key] = value
    return result


def verify(raw, expected_digest, expected_source):
    if (not isinstance(raw, bytes) or len(raw) > MAX_ARCHIVE
            or not isinstance(expected_digest, str)
            or not re.fullmatch('[0-9a-f]{64}', expected_digest)
            or not isinstance(expected_source, str)
            or not re.fullmatch('[0-9a-f]{40}', expected_source)
            or hashlib.sha256(raw).hexdigest() != expected_digest):
        raise Invalid('independently pinned package bytes and source required')
    expected = {name + '.py' for name in module_inventory(expected_source)} | {'manifest.json'}
    files = {}
    try:
        with tarfile.open(fileobj=io.BytesIO(raw), mode='r:') as archive:
            for member in archive:
                if (member.name not in expected or member.name in files
                        or not member.isfile() or member.mode != 0o644
                        or not 0 <= member.size <= (65536 if member.name == 'manifest.json' else LIMIT)):
                    raise Invalid('invalid package member')
                files[member.name] = archive.extractfile(member).read(member.size + 1)
                if len(files[member.name]) != member.size:
                    raise Invalid('truncated package member')
    except (tarfile.TarError, OSError):
        raise Invalid('invalid package archive') from None
    if set(files) != expected:
        raise Invalid('incomplete package')
    try:
        manifest = json.loads(files.pop('manifest.json'), object_pairs_hook=pairs)
    except (ValueError, UnicodeError):
        raise Invalid('invalid package manifest') from None
    if (not isinstance(manifest, dict) or set(manifest) != FIELDS
            or type(manifest['schema']) is not int or manifest['schema'] != 1
            or manifest['source'] != expected_source
            or manifest['scope'] != 'declared-deployment-helper-static-imports'
            or any(manifest[key] is not False for key in
                   ('installation_ready', 'closure_verified', 'rendered_operation_verified'))):
        raise Invalid('unsupported package authority or identity')
    hashes = {name: hashlib.sha256(body).hexdigest() for name, body in files.items()}
    imports = inspect_sources({name[:-3]: body for name, body in files.items()}, expected_source)
    if manifest['sha256'] != hashes or manifest['local_imports'] != imports:
        raise Invalid('package contents disagree with manifest')
    # These are required follow-up obligations, not evidence supplied by a package.
    return {'source': expected_source, 'package_sha256': expected_digest,
            'verified_modules': len(files), 'static_imports_verified': True,
            'required_platform_roles': ['python-posix-runtime', 'kubernetes-cli',
                                        'containerd-cli', 'systemd-cli'],
            'platform_verified': False, 'rendered_operation_verified': False,
            'generation_switch_ready': False, 'installation_ready': False}
