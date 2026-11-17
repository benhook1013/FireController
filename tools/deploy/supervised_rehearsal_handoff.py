"""Uncalled producer handoff seam. Evidence only; never removes artifacts."""
import fcntl
import os
from pathlib import Path
import supervised_failure as supervisor
import rehearsal_operation as operation
import failed_equivalent as failure
from artifact_retention import Refuse


def observe_failed_handoff(args, consumer, command):
    """Requires producer-owned fresh work and outer source/budget custody.

    Holds consumer custody on return or raises on failed reacquisition. Evidence
    is not a disposal instruction. No bounded producer or CLI calls this seam.
    """
    work=Path(args['root'])/args['operation'];lease=Path(args['lease'])
    started=failure.read_record(work/'started.json')
    expected={'version':1,'kind':'replayable-rehearsal-not-enrolled',
        'source_path':str(args['source']),'source_sha256':args['source_sha256'],
        'runner_path':str(args['runner']),'runner_sha256':args['runner_sha256'],
        'source_revision':args['revision'],'operation':args['operation'],'root':str(args['root']),
        'lease_identity':supervisor.ident(lease)}
    if started!=expected:raise Refuse('handoff start identity changed')
    opened=os.fstat(consumer.fileno())
    if [opened.st_dev,opened.st_ino]!=expected['lease_identity']:raise Refuse('handoff consumer identity changed')
    fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
    fcntl.flock(consumer,fcntl.LOCK_UN)
    try:
        code,observed=supervisor.run_observed(work,lease,expected['lease_identity'],command)
    finally:
        # A consumer still holding this lease makes outcome unresolved, not safe.
        fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
    terminal=supervisor.qualify_failure(work,lease,consumer,observed,code)
    if failure.read_record(work/'started.json')!=expected:raise Refuse('handoff start changed during service')
    limit=args.get('max_database_bytes',1024**3)
    operation.checked_file(args['source'],args['source_sha256'],limit,args.get('source_owner_uid'))
    operation.checked_file(args['runner'],args['runner_sha256'],128*1024**2)
    output=work/'data/firecontroller.sqlite'
    output_hash,output_stat=operation.digest(output)
    operation.checked_file(output,output_hash,limit)
    proof={'version':1,'state':'qualified_supervised_failure_evidence_only',
        'started':expected,'supervisor':terminal,'output_sha256':output_hash,
        'output_identity':[output_stat.st_dev,output_stat.st_ino]}
    operation.new_json(work/'supervised-failure.json',proof)
    return proof
