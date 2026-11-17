"""Qualify a deliberately narrow direct-service observation before graph joining.

No unit editing or process execution. These checks only recognize the observed
simple host-namespace service shape; unsupported context remains unresolved.
They do not establish the complete effective environment or package compatibility.
"""
from service_roots import inspect_roots, Invalid

EMPTY_PROPERTIES = {
    'RootDirectory': ('s', ''), 'RootImage': ('s', ''),
    'Environment': ('as', []), 'EnvironmentFiles': ('a(sb)', []),
    'PassEnvironment': ('as', []), 'UnsetEnvironment': ('as', []),
    'BindPaths': ('a(ssbt)', []), 'BindReadOnlyPaths': ('a(ssbt)', []),
    'ExecStartPre': ('a(sasbttttuii)', []),
    'ExecStartPost': ('a(sasbttttuii)', []),
    'ExecStop': ('a(sasbttttuii)', []),
    'ExecStopPost': ('a(sasbttttuii)', []),
    'LoadCredential': ('a(ss)', []),
    'LoadCredentialEncrypted': ('a(ss)', []),
    'SetCredential': ('a(say)', []),
    'SetCredentialEncrypted': ('a(say)', []),
}


def context_checked(snapshot):
    if not isinstance(snapshot, dict) or set(snapshot) != {'ExecStart', *EMPTY_PROPERTIES}:
        raise Invalid('complete supported service observation required')
    for name, (kind, empty) in EMPTY_PROPERTIES.items():
        value = snapshot[name]
        if not isinstance(value, dict) or set(value) != {'type', 'data'} or value['type'] != kind:
            raise Invalid('invalid service context observation')
        if value['data'] != empty:
            # Do not echo values: environment and credential paths are private.
            raise Invalid('service context requires additional dependency resolution')
    return snapshot['ExecStart']


def inspect_service_graph(batch, health):
    roots = inspect_roots(context_checked(batch), context_checked(health))
    for name, value in roots.items():
        if isinstance(value, str) and ('$' in value or '%' in value):
            raise Invalid('unresolved service substitution')
    # Effective manager defaults, credentials, additional mounts, stop commands,
    # unit/drop-in bytes and script/provider graph are not supplied here. Never
    # allow this recognized partial context to become installation authority.
    return {**roots, 'context_scope': 'direct-launch-properties-only',
            'unresolved': ['effective manager environment and credentials',
                           'complete mount and service lifecycle properties',
                           'unit and drop-in provenance and custody',
                           'publisher provider configurations and readers',
                           'executable library and runtime dependency closure']}
