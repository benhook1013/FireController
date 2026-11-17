"""Join observed service roots and canonical configuration inventory, read-only.

Inputs are private observations, not install authority. This module never reads
paths or runs an executable. Publisher transitive dependencies stay unresolved.
"""
from service_context import inspect_service_graph
from service_roots import Invalid, path

SHARED = {'batch_configuration', 'batch_state', 'source', 'phase_lease'}
PROVIDER = {'provider_configuration', 'provider_state', 'transport_executable', 'transport_configuration'}
STATES = {'present', 'missing', 'unavailable', 'symlink', 'wrong_type', 'not_private', 'not_executable'}


def join_inventory(batch, health, inspection):
    roots = inspect_service_graph(batch, health)
    if (not isinstance(inspection, dict)
            or set(inspection) != {'dependencies', 'installation_ready'}
            or inspection['installation_ready'] is not False
            or not isinstance(inspection['dependencies'], list)
            or not 7 <= len(inspection['dependencies']) <= 1028):
        raise Invalid('bounded canonical configuration observation required')
    seen = {}
    providers = {}
    dependencies = []
    for item in inspection['dependencies']:
        if not isinstance(item, dict) or set(item) != {'role', 'provider', 'path', 'state'}:
            raise Invalid('invalid dependency observation')
        role, index, state = item['role'], item['provider'], item['state']
        if (not isinstance(role, str) or type(index) is not int
                or not isinstance(state, str) or state not in STATES
                or (role in SHARED and index != -1)
                or (role in PROVIDER and not 0 <= index < 256)
                or role not in SHARED | PROVIDER):
            raise Invalid('invalid dependency role or state')
        observed_path = path(item['path'])
        key = (role, index)
        if key in seen:
            raise Invalid('duplicate dependency observation')
        seen[key] = observed_path
        if index >= 0:
            providers.setdefault(index, set()).add(role)
        dependencies.append(dict(item))
    if (any((role, -1) not in seen for role in SHARED - {'phase_lease'})
            or not providers or set(providers) != set(range(len(providers)))
            or any(roles != PROVIDER for roles in providers.values())):
        raise Invalid('incomplete configuration dependency inventory')
    if seen['batch_configuration', -1] != roots['batch_configuration']:
        raise Invalid('service and configuration observations disagree')
    # Do not deduplicate executable roles by path or replace a historical helper
    # with the batch executable. Their compatibility obligations are independent.
    return {'services': roots, 'configuration_dependencies': dependencies,
            'observation_scope': 'direct-service-and-batch-configuration',
            'unresolved': list(roots['unresolved']) + [
                'observation freshness, common instance and namespace binding',
                'transport INI transitive references and executable provenance',
                'managed producer, restore and rollback generation compatibility'],
            'closure_verified': False, 'installation_ready': False}
