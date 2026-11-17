#!/usr/bin/env python3
"""Render the mandatory bounded rehearsal entrypoint for a deployment preparer."""
import json
from pathlib import Path
from artifact_retention import Refuse

FIELDS = {'root', 'budget_lease', 'source_lease', 'source_lease_uid', 'source_owner_uid', 'lease'}
INPUTS = {'source', 'source_sha256', 'runner', 'runner_sha256', 'revision', 'operation'}


def render(config, policy, package):
    if not isinstance(config, dict) or not isinstance(policy, dict) or set(policy) != FIELDS:
        raise Refuse('explicit bounded rehearsal policy required')
    package = Path(package)
    if not package.is_absolute() or package.resolve() != package:
        raise Refuse('canonical helper package required')
    if config.get('mode') == 'schema-unchanged':
        if set(config) != {'mode', 'proof'} or not isinstance(config['proof'], dict):
            raise Refuse('explicit schema-unchanged proof required')
        proof = config['proof']
        if proof.get('state') != 'passed' or proof.get('storage_schema_unchanged') is not True or proof.get('new_populated_rehearsal') is not False:
            raise Refuse('schema-unchanged proof does not permit skipping')
        # Preserve evidence; the deployment owner must qualify its source comparison.
        return ('# Schema-unchanged deployment: no populated database copied.\n'
                + 'import json\nprint(json.dumps(' + repr(proof) + ', sort_keys=True))\n')
    if set(config) != INPUTS | {'mode'} or config['mode'] != 'bounded':
        raise Refuse('explicit bounded inputs or schema-unchanged mode required')
    args = dict(policy, **{k:config[k] for k in INPUTS}, supervised=True)
    return ('import sys,json\n' + 'sys.path.insert(0, ' + repr(str(package)) + ')\n'
            + 'from bounded_rehearsal import execute\n'
            + 'print(json.dumps(execute(**' + repr(args) + '), sort_keys=True))\n')
