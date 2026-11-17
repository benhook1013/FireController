"""Package exact Git deployment-helper bytes, not an executable operation.

Only declared source modules and their static imports are qualified. External
programs, generated templates and live custody remain separate obligations.
"""
import ast
import hashlib
import io
import json
import re
import subprocess
import sys
import tarfile

MODULES = frozenset('artifact_operation artifact_retention artifact_schedule bounded_rehearsal coordinator_split_adapter failed_equivalent failed_equivalent_repair image_layers kubernetes prepare_rehearsal prepare_successor prepared_image_content prepared_release rehearsal_operation render_split_coordinator supervised_equivalent supervised_failure'.split())
LIMIT = 4 << 20


class Invalid(ValueError):
    pass


def inspect_sources(sources):
    if set(sources) != MODULES:
        raise Invalid('exact deployment module inventory required')
    imports = {}
    for name, raw in sorted(sources.items()):
        if not isinstance(raw, bytes) or len(raw) > LIMIT:
            raise Invalid('bounded source bytes required')
        try:
            tree = ast.parse(raw)
        except (SyntaxError, ValueError):
            raise Invalid('invalid Python source') from None
        local = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                names = [item.name.split('.')[0] for item in node.names]
            elif isinstance(node, ast.ImportFrom):
                if node.level or not node.module:
                    raise Invalid('relative imports need explicit qualification')
                names = [node.module.split('.')[0]]
            else:
                continue
            for imported in names:
                if imported in MODULES:
                    local.add(imported)
                elif imported not in sys.stdlib_module_names:
                    raise Invalid('unqualified external Python dependency')
        imports[name] = sorted(local)
    return imports


def source_at(repository, revision):
    if not isinstance(revision, str) or not re.fullmatch('[0-9a-f]{40}', revision):
        raise Invalid('full immutable Git revision required')
    def git(*args):
        return subprocess.run(['git', '--no-replace-objects', '-C', str(repository), *args], check=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              timeout=30).stdout
    if git('rev-parse', revision + '^{commit}').decode().strip() != revision:
        raise Invalid('exact commit required')
    tracked = git('ls-tree', '-r', '--name-only', revision, '--', 'tools/deploy').decode().splitlines()
    actual = {path.removeprefix('tools/deploy/').removesuffix('.py')
              for path in tracked if path.endswith('.py')
              and not path.removeprefix('tools/deploy/').startswith('test_')
              and path != 'tools/deploy/split_test_fixture.py'}
    if actual != MODULES:
        raise Invalid('deployment source inventory changed; explicit review required')
    sources = {}
    for name in sorted(MODULES):
        path = 'tools/deploy/' + name + '.py'
        entry = git('ls-tree', revision, '--', path).decode().strip().split()
        if len(entry) != 4 or entry[0] not in ('100644', '100755') or entry[1] != 'blob' or entry[3] != path:
            raise Invalid('regular tracked deployment source required')
        size = int(git('cat-file', '-s', entry[2]))
        if not 0 <= size <= LIMIT:
            raise Invalid('source exceeds package bound')
        raw = git('cat-file', 'blob', entry[2])
        if len(raw) != size:
            raise Invalid('Git object size mismatch')
        sources[name] = raw
    return sources


def assemble(repository, revision):
    sources = source_at(repository, revision)
    imports = inspect_sources(sources)
    manifest = {
        'schema': 1, 'source': revision,
        'scope': 'declared-deployment-helper-static-imports',
        'sha256': {name + '.py': hashlib.sha256(raw).hexdigest()
                   for name, raw in sorted(sources.items())},
        'local_imports': imports,
        'python_version_used_for_static_inventory': list(sys.version_info[:2]),
        'unresolved': ['Python interpreter and standard library runtime',
                       'external executables and generated operation templates',
                       'configuration credentials service namespace and custody',
                       'coordinated generation switch rollback and recovery'],
        'rendered_operation_verified': False,
        'closure_verified': False, 'installation_ready': False,
    }
    files = {name + '.py': raw for name, raw in sources.items()}
    files['manifest.json'] = (json.dumps(manifest, sort_keys=True, indent=2) + '\n').encode()
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode='w', format=tarfile.USTAR_FORMAT) as archive:
        for name, raw in sorted(files.items()):
            member = tarfile.TarInfo(name)
            member.mode = 0o644
            member.size = len(raw)
            archive.addfile(member, io.BytesIO(raw))
    return output.getvalue(), manifest
