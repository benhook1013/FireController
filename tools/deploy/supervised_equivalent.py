"""Explicit fresh supervised duplicate disposal; never adopts old operations."""
import fcntl
import os
from pathlib import Path
import stat
import failed_equivalent as common
import rehearsal_operation as operation
import supervised_failure as supervisor
from artifact_retention import Refuse, open_lease

EMPTY_DIRS={'dev','etc','proc','root','run','sys','tmp','usr','var'}
RECORDS={'started.json','supervisor-start.json','supervisor-observed.json','supervised-failure.json'}
INTENT='supervised-disposal-intent.json'
RECEIPT='supervised-equivalent-disposal.json'


def validate_record(work, proof):
    fields=common.START_FIELDS|{'state','exit_code','supervisor','output_sha256','output_identity'}
    if not isinstance(proof,dict) or set(proof)!=fields or proof['state']!='qualified_supervised_failure_evidence_only':
        raise Refuse('invalid supervised failure schema')
    started=common.read_record(work/'started.json')
    terminal=proof['supervisor']
    if not isinstance(terminal,dict) or type(terminal.get('exit_code')) is not int or terminal.get('state')!='known_failed_unit' or terminal.get('exit_code')!=proof['exit_code']:
        raise Refuse('invalid supervised terminal binding')
    # Reuse the strict shared field validator only for scalar/start identity
    # validation. This does not turn supervised evidence into foreground authority.
    typed={k:proof[k] for k in common.START_FIELDS}
    typed.update(state='failed_foreground_reaped',exit_code=proof['exit_code'],finished_at=0,
        work_identity=terminal.get('work_identity'),output_identity=proof['output_identity'],output_sha256=proof['output_sha256'])
    common.validate_proof(typed,started)
    if proof['root']!=str(work.parent) or proof['operation']!=work.name or terminal.get('work')!=str(work):
        raise Refuse('supervised operation mismatch')
    observed=common.read_record(work/'supervisor-observed.json')
    start=common.read_record(work/'supervisor-start.json')
    if type(observed.get('version')) is not int or observed.get('version')!=1 or type(start.get('version')) is not int:raise Refuse('invalid supervised binding version')
    if terminal!=dict(observed,state='known_failed_unit',exit_code=proof['exit_code']) or start!={k:v for k,v in observed.items() if k not in ('invocation','cgroup')}:
        raise Refuse('supervised observation changed')
    if observed.get('unit')!=supervisor.unit_for(work) or observed.get('cgroup')!='/system.slice/'+supervisor.unit_for(work):
        raise Refuse('supervised unit mismatch')
    return observed


def inventory(work, *, removed):
    if not (work/'data').is_dir() or (work/'data').is_symlink():raise Refuse('missing sandbox data directory')
    allowed=RECORDS|EMPTY_DIRS|{'data','artifact.lease','rehearsal.json',INTENT,RECEIPT}
    if not removed:allowed.add('migration-runner')
    for p in work.iterdir():
        s=p.lstat()
        if p.name not in allowed or stat.S_ISLNK(s.st_mode) or s.st_uid!=os.geteuid():raise Refuse('unknown sandbox artifact')
        if p.name in EMPTY_DIRS:
            if not stat.S_ISDIR(s.st_mode) or any(p.iterdir()):raise Refuse('nonempty sandbox mountpoint')
        elif p.name=='data':
            expected=[] if removed else ['firecontroller.sqlite']
            if not stat.S_ISDIR(s.st_mode) or sorted(x.name for x in p.iterdir())!=expected:raise Refuse('unknown sandbox data')
        elif p.name=='migration-runner':
            if not stat.S_ISREG(s.st_mode) or s.st_nlink!=1:raise Refuse('unsafe staged runner')
        elif p.name=='artifact.lease':
            if not stat.S_ISREG(s.st_mode) or s.st_size!=0 or s.st_nlink!=1:raise Refuse('unsafe lease mountpoint')
        elif not stat.S_ISREG(s.st_mode) or s.st_size>operation.MAX_REPORT or s.st_nlink!=1:
            raise Refuse('unsafe retained failure proof')


def sync(path):
    fd=os.open(path,os.O_RDONLY|os.O_DIRECTORY)
    try:os.fsync(fd)
    finally:os.close(fd)


def dispose(args, failure, held_locks):
    if not isinstance(failure,operation.KnownSupervisedFailure):raise Refuse('fresh supervised invocation required')
    work=Path(args['root'])/args['operation'];proof=failure.evidence
    observed=validate_record(work,proof)
    if proof['output_sha256']!=args['source_sha256']:return 0
    expected={'root':str(args['root']),'operation':args['operation'],'source_path':str(args['source']),
        'source_sha256':args['source_sha256'],'runner_path':str(args['runner']),'runner_sha256':args['runner_sha256'],'source_revision':args['revision']}
    if any(proof[k]!=v for k,v in expected.items()):raise Refuse('supervised argument mismatch')
    output=work/'data/firecontroller.sqlite';runner_copy=work/'migration-runner'
    with open_lease(args['lease']) as consumer:
        fcntl.flock(consumer,fcntl.LOCK_EX|fcntl.LOCK_NB)
        def custody():
            for path,fd in held_locks+[(args['lease'],consumer)]:
                s=os.fstat(fd.fileno())
                if common.identity(path)!=[s.st_dev,s.st_ino]:raise Refuse('supervised lease replaced')
            if common.read_record(work/'supervised-failure.json')!=proof:raise Refuse('supervised failure changed')
            validate_record(work,proof)
            if supervisor.qualify_failure(work,Path(args['lease']),consumer,observed,proof['exit_code'])!=proof['supervisor']:
                raise Refuse('supervised terminal identity changed')
            for suffix in ('-wal','-shm','-journal'):
                if Path(str(args['source'])+suffix).exists():raise Refuse('source sidecar')
            operation.checked_file(args['source'],args['source_sha256'],args.get('max_database_bytes',1024**3),args.get('source_owner_uid'))
            operation.checked_file(args['runner'],args['runner_sha256'],128*1024**2)
        custody();inventory(work,removed=False)
        _,db=operation.checked_file(output,args['source_sha256'],args.get('max_database_bytes',1024**3))
        _,runner=operation.checked_file(runner_copy,args['runner_sha256'],128*1024**2)
        if [db.st_dev,db.st_ino]!=proof['output_identity'] or db.st_nlink!=1 or runner.st_nlink!=1:
            raise Refuse('supervised duplicate identity changed')
        if common.identity(args['source'])==[db.st_dev,db.st_ino] or common.identity(args['runner'])==[runner.st_dev,runner.st_ino]:raise Refuse('protected input aliases duplicate')
        intent={'version':1,'state':'prepared','evidence':proof,'database_bytes':db.st_size,'runner_bytes':runner.st_size,'runner_identity':[runner.st_dev,runner.st_ino]}
        operation.new_json(work/INTENT,intent)
        custody();inventory(work,removed=False)
        if common.identity(output)!=proof['output_identity']:raise Refuse('database identity changed')
        operation.checked_file(output,args['source_sha256'],args.get('max_database_bytes',1024**3))
        output.unlink();sync(work/'data')
        custody()
        if common.identity(runner_copy)!=intent['runner_identity']:raise Refuse('runner identity changed')
        operation.checked_file(runner_copy,args['runner_sha256'],128*1024**2)
        runner_copy.unlink();sync(work)
        inventory(work,removed=True)
        operation.new_json(work/RECEIPT,dict(intent,state='verified_supervised_equivalents_removed'))
        return db.st_size+runner.st_size


def verify_disposed(work):
    receipt=common.read_record(work/RECEIPT);intent=common.read_record(work/INTENT)
    fields={'version','state','evidence','database_bytes','runner_bytes','runner_identity'}
    if (not isinstance(receipt,dict) or set(receipt)!=fields or type(receipt['version']) is not int or receipt['version']!=1
            or receipt['state']!='verified_supervised_equivalents_removed' or type(intent.get('version')) is not int
            or intent!=dict(receipt,state='prepared')):raise Refuse('invalid supervised disposition')
    if any(type(receipt[k]) is not int or receipt[k]<=0 for k in ('database_bytes','runner_bytes')):raise Refuse('invalid supervised byte counts')
    rid=receipt['runner_identity']
    if not isinstance(rid,list) or len(rid)!=2 or any(type(x) is not int or x<0 for x in rid):raise Refuse('invalid runner identity')
    proof=common.read_record(work/'supervised-failure.json')
    validate_record(work,proof)
    if receipt['evidence']!=proof or proof['output_sha256']!=proof['source_sha256']:raise Refuse('invalid supervised equivalence')
    inventory(work,removed=True)
