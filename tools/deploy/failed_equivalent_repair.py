"""Bounded completion of an already-unlinked equivalent output; never deletes."""
from contextlib import ExitStack
import bounded_rehearsal
import fcntl
import os
from pathlib import Path
import failed_equivalent as failed
import rehearsal_operation as operation
from artifact_retention import Refuse, open_lease


def settle_absent(args, *, budget_lease, source_lease, source_lease_uid=None):
    paths=[str(budget_lease),str(source_lease),str(args['lease'])]
    if len(set(paths))!=3:raise Refuse('separate repair leases required')
    with ExitStack() as stack:
        budget=stack.enter_context(open_lease(budget_lease))
        fcntl.flock(budget,fcntl.LOCK_EX|fcntl.LOCK_NB)
        source=stack.enter_context(bounded_rehearsal.open_source_lease(source_lease,os.geteuid() if source_lease_uid is None else source_lease_uid))
        fcntl.flock(source,fcntl.LOCK_EX|fcntl.LOCK_NB)
        return _settle_absent(args,[(budget_lease,budget),(source_lease,source)])


def _settle_absent(args, held_locks):
    root=Path(args['root']);work=root/args['operation'];output=work/'data/firecontroller.sqlite'
    with open_lease(args['lease']) as consumer:
        fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
        proof=failed.read_record(work/'failure.json')
        failed.validate_proof(proof,failed.read_record(work/'started.json'))
        intent=failed.read_record(work/'failed-disposal-intent.json')
        if set(intent)!={'version','state','evidence','bytes'} or type(intent['version']) is not int or intent['version']!=1 or intent['state']!='failed_equivalent_disposal_prepared' or intent['evidence']!=proof:
            raise Refuse('unqualified interrupted intent')
        expected={'root':str(root),'operation':args['operation'],'source_path':str(args['source']),
            'source_sha256':args['source_sha256'],'runner_path':str(args['runner']),
            'runner_sha256':args['runner_sha256'],'source_revision':args['revision']}
        if any(proof.get(k)!=v for k,v in expected.items()) or proof['output_sha256']!=args['source_sha256']:
            raise Refuse('repair invocation mismatch')
        for path,fd in held_locks+[(args['lease'],consumer)]:
            s=os.fstat(fd.fileno())
            if failed.identity(path)!=[s.st_dev,s.st_ino]:raise Refuse('repair custody changed')
        if failed.identity(args['lease'])!=proof['lease_identity'] or failed.identity(work)!=proof['work_identity']:
            raise Refuse('repair operation identity changed')
        allowed={'started.json','failure.json','rehearsal.json','data','failed-disposal-intent.json'}
        if any(p.name not in allowed or p.is_symlink() for p in work.iterdir()):raise Refuse('unknown repair artifact')
        if output.exists() or not (work/'data').is_dir() or any((work/'data').iterdir()):
            raise Refuse('repair never removes remaining output')
        for suffix in ('-wal','-shm','-journal'):
            if Path(str(args['source'])+suffix).exists():raise Refuse('repair source sidecar')
        _,source=operation.checked_file(args['source'],args['source_sha256'],args.get('max_database_bytes',1024**3),args.get('source_owner_uid'))
        operation.checked_file(args['runner'],args['runner_sha256'],128*1024**2)
        if type(intent['bytes']) is not int or intent['bytes']!=source.st_size:raise Refuse('repair byte identity mismatch')
        # Reestablish directory durability after a crash before the unlink sync.
        fd=os.open(work/'data',os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
        operation.new_json(work/'failed-equivalent-disposal.json',dict(intent,state='failed_equivalent_output_removed'))
        return {'state':'failed_equivalent_output_removed','failure_preserved':True}
