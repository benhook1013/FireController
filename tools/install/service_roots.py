"""Decode observed systemd entrypoints into private installation graph roots.

No subprocesses, files or network. Inputs must be fresh typed D-Bus observations,
not text from systemctl show or declarations inferred from a release name. This
only recognizes the selected batch/publisher launch shapes, not arbitrary units.
"""
from pathlib import PurePosixPath
import re


class Invalid(ValueError):
    pass


def path(value):
    if (not isinstance(value, str) or not 1 <= len(value) <= 4096
            or any(ord(c) < 32 or ord(c) == 127 for c in value)
            or not value.startswith('/') or str(PurePosixPath(value)) != value
            or '..' in PurePosixPath(value).parts):
        raise Invalid('unsupported dependency path')
    return value


def argv(observation):
    if (not isinstance(observation, dict) or set(observation) != {'type', 'data'}
            or observation['type'] != 'a(sasbttttuii)'
            or not isinstance(observation['data'], list) or len(observation['data']) != 1):
        raise Invalid('one typed ExecStart observation required')
    entry = observation['data'][0]
    if (not isinstance(entry, list) or len(entry) != 10 or entry[2] is not False
            or any(type(n) is not int for n in entry[3:])
            or not isinstance(entry[1], list) or not 1 <= len(entry[1]) <= 16
            or any(not isinstance(x, str) or len(x) > 4096 for x in entry[1])):
        raise Invalid('unsupported ExecStart entry')
    executable = path(entry[0])
    if entry[1][0] != executable:
        raise Invalid('alternate argv identity unsupported')
    return executable, entry[1]


def inspect_roots(batch, health):
    """Observe direct roots; no compatibility/custody/install/delete authority.

    Service namespaces, environment, drop-ins, timers, ExecStartPre/Post and
    publisher script's configured provider roots still need independent closure.
    In particular this never substitutes the batch binary for a distinct reader.
    """
    batch_exe, b = argv(batch)
    health_exe, h = argv(health)
    if len(b) != 4 or b[1:3] != ['backup-push-batch', '--config']:
        raise Invalid('unsupported batch invocation')
    batch_config = path(b[3])
    # The qualified versioned publisher renderer uses exactly Python's isolated,
    # no-site, no-bytecode prefix. Preserve this as observed context rather than
    # silently interpreting arbitrary interpreter flags or dropping a wrapper.
    isolated = len(h) == 9 and h[1:4] == ['-I', '-S', '-B']
    health_args = [h[0], *h[4:]] if isolated else h
    if len(health_args) != 6 or health_args[2] != '--fcd' or health_args[4] != '--sha256':
        raise Invalid('unsupported health invocation')
    script, helper = path(health_args[1]), path(health_args[3])
    if not re.fullmatch('[0-9a-f]{64}', health_args[5]):
        raise Invalid('missing health helper pin')
    return {'batch_executable': batch_exe, 'batch_configuration': batch_config,
            'health_interpreter': health_exe, 'health_script': script,
            'health_helper': helper, 'health_helper_sha256': health_args[5],
            'health_interpreter_flags': ['-I', '-S', '-B'] if isolated else [],
            'closure_verified': False, 'installation_ready': False}
