"""Opt-in producer-owned equivalent failure output, never historical adoption."""
import fcntl
import json
import os
import re
import math
import stat
from pathlib import Path
import rehearsal_operation as operation
from artifact_retention import Refuse, open_lease


def identity(path):
    s=Path(path).lstat()
    return [s.st_dev,s.st_ino]


def read_record(path):
    s=path.lstat()
    if not stat.S_ISREG(s.st_mode) or s.st_size>16384 or s.st_uid!=os.geteuid() or s.st_mode & 0o077:
        raise Refuse('unsafe failure record')
    return json.loads(path.read_text())


START_FIELDS={'version','kind','source_path','source_sha256','runner_path','runner_sha256','source_revision','operation','root','lease_identity'}
FAIL_FIELDS=START_FIELDS|{'state','exit_code','finished_at','work_identity','output_identity','output_sha256'}


def validate_proof(proof, started):
    if (not isinstance(proof,dict) or set(proof)!=FAIL_FIELDS
            or not isinstance(started,dict) or set(started)!=START_FIELDS
            or started!={k:proof[k] for k in START_FIELDS}
            or type(proof['version']) is not int or proof['version']!=1
            or proof['kind']!='replayable-rehearsal-not-enrolled'
            or proof['state']!='failed_foreground_reaped'
            or type(proof['exit_code']) is not int or proof['exit_code']==0
            or type(proof['finished_at']) not in (int,float) or not math.isfinite(proof['finished_at'])):
        raise Refuse('invalid complete failure identity')
    for name in ('source_sha256','runner_sha256','output_sha256'):
        if not isinstance(proof[name],str) or not re.fullmatch('[0-9a-f]{64}',proof[name]):raise Refuse('invalid failure digest')
    if not isinstance(proof['source_revision'],str) or not re.fullmatch('[0-9a-f]{40}',proof['source_revision']):raise Refuse('invalid failure revision')
    if not isinstance(proof['operation'],str) or not re.fullmatch('[a-zA-Z0-9_-]{1,80}',proof['operation']):raise Refuse('invalid failure operation')
    for name in ('source_path','runner_path','root'):
        if not isinstance(proof[name],str) or not Path(proof[name]).is_absolute():raise Refuse('invalid failure path')
    for name in ('lease_identity','work_identity','output_identity'):
        value=proof[name]
        if not isinstance(value,list) or len(value)!=2 or any(type(n) is not int or n<0 for n in value):raise Refuse('invalid failure inode')


def dispose(args, failure, held_locks):
    # Only the catch site surrounding this invocation may call this helper.
    # The durable record alone is deliberately not an enrollment API.
    if not isinstance(failure, operation.KnownForegroundFailure):
        raise Refuse('known invocation failure required')
    proof=failure.evidence
    root=Path(args['root']);work=root/args['operation']
    validate_proof(proof,read_record(work/'started.json'))
    output=work/'data/firecontroller.sqlite'
    limit=args.get('max_database_bytes',1024**3)
    if args.get('supervised') or proof['state']!='failed_foreground_reaped':
        raise Refuse('supervised failure remains unresolved')
    expected={'root':str(root),'operation':args['operation'],'source_path':str(args['source']),
        'source_sha256':args['source_sha256'],'runner_path':str(args['runner']),
        'runner_sha256':args['runner_sha256'],'source_revision':args['revision']}
    if any(proof.get(k)!=v for k,v in expected.items()):
        raise Refuse('failure invocation identity mismatch')
    if proof['output_sha256']!=args['source_sha256']:
        return 0  # Unique failure state is diagnostic/recovery material.
    with open_lease(args['lease']) as consumer:
        fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
        def validate():
            for path,fd in held_locks+[(args['lease'],consumer)]:
                s=os.fstat(fd.fileno())
                if identity(path)!=[s.st_dev,s.st_ino]:raise Refuse('custody lease replaced')
            if identity(args['lease'])!=proof['lease_identity'] or identity(work)!=proof['work_identity']:
                raise Refuse('failure custody identity changed')
            if read_record(work/'failure.json')!=proof:raise Refuse('failure proof changed')
            started=read_record(work/'started.json')
            validate_proof(proof,started)
            allowed={'started.json','failure.json','rehearsal.json','data','failed-disposal-intent.json'}
            if any(p.name not in allowed or p.is_symlink() for p in work.iterdir()):raise Refuse('unexpected failure artifact')
            if sorted(p.name for p in (work/'data').iterdir())!=['firecontroller.sqlite']:
                raise Refuse('unexpected data or sidecar')
            for suffix in ('-wal','-shm','-journal'):
                if Path(str(args['source'])+suffix).exists():raise Refuse('source sidecar')
            operation.checked_file(args['source'],args['source_sha256'],limit,args.get('source_owner_uid'))
            operation.checked_file(args['runner'],args['runner_sha256'],128*1024**2)
            _,s=operation.checked_file(output,args['source_sha256'],limit)
            if [s.st_dev,s.st_ino]!=proof['output_identity'] or s.st_nlink!=1:
                raise Refuse('failure output identity changed')
            if identity(args['source'])==[s.st_dev,s.st_ino]:raise Refuse('source aliases output')
            return s
        info=validate()
        intent={'version':1,'state':'failed_equivalent_disposal_prepared','evidence':proof,'bytes':info.st_size}
        operation.new_json(work/'failed-disposal-intent.json',intent)
        validate()
        output.unlink()
        fd=os.open(output.parent,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
        operation.new_json(work/'failed-equivalent-disposal.json',dict(intent,state='failed_equivalent_output_removed'))
        return info.st_size


def verify_disposed(work):
    receipt=read_record(work/'failed-equivalent-disposal.json')
    intent=read_record(work/'failed-disposal-intent.json')
    proof=read_record(work/'failure.json')
    validate_proof(proof,read_record(work/'started.json'))
    if (set(receipt)!={'version','state','evidence','bytes'} or type(receipt['version']) is not int or receipt['version']!=1
            or receipt['state']!='failed_equivalent_output_removed'
            or receipt['evidence']!=proof or type(receipt['bytes']) is not int or receipt['bytes']<=0
            or type(intent.get('version')) is not int or intent!=dict(receipt,state='failed_equivalent_disposal_prepared')
            or proof.get('operation')!=work.name or proof.get('root')!=str(work.parent)
            or proof.get('state')!='failed_foreground_reaped'
            or proof.get('output_sha256')!=proof.get('source_sha256')):
        raise Refuse('unproven failed disposition')
    allowed={'started.json','failure.json','rehearsal.json','data','failed-disposal-intent.json','failed-equivalent-disposal.json'}
    if any(p.name not in allowed or p.is_symlink() for p in work.iterdir()):raise Refuse('unexpected retained failure artifact')
    if not (work/'data').is_dir() or any((work/'data').iterdir()):raise Refuse('unresolved failed data')
