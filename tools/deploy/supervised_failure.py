"""Observe a fresh producer-owned service; no unit or artifact cleanup."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time
from artifact_retention import Refuse
from rehearsal_operation import new_json

FIELDS=('LoadState','ActiveState','SubState','InvocationID','ControlGroup','MainPID','ControlPID','Result','ExecMainStatus')

def unit_for(work):
    return 'fc-rehearsal-'+hashlib.sha256(str(work).encode()).hexdigest()[:24]+'.service'

def properties(unit):
    result=subprocess.run(['/usr/bin/systemctl','show',unit,'--no-pager']+['--property='+x for x in FIELDS],capture_output=True,text=True,timeout=5,check=True)
    if len(result.stdout)>16384:raise Refuse('supervisor observation oversized')
    values={}
    for line in result.stdout.splitlines():
        key,value=line.split('=',1)
        if key not in FIELDS or key in values:raise Refuse('supervisor observation schema')
        values[key]=value
    if set(values)!=set(FIELDS):raise Refuse('incomplete supervisor observation')
    return values


def ident(path):
    s=Path(path).lstat();return [s.st_dev,s.st_ino]


def run_observed(work, lease, lease_identity, command, *, timeout=210):
    """Caller owns freshly created private work and producer budget/source custody.

    Caller releases consumer lease for service handoff, then reacquires it before
    qualifying the returned binding. This function never declares safe disposal.
    """
    work=Path(work);unit=unit_for(work)
    if not work.is_absolute() or work.resolve()!=work or ident(lease)!=lease_identity:
        raise Refuse('supervisor operation identity')
    # Exact launch identity, never external generic shell or alternate unit.
    if (not command or command[0]!='/usr/bin/systemd-run' or '--collect' in command
            or command.count('--unit='+unit)!=1 or '--wait' not in command
            or any(x.startswith('--unit=') and x!='--unit='+unit for x in command)):
        raise Refuse('retained exact supervisor command required')
    if properties(unit)['LoadState']!='not-found':raise Refuse('supervisor unit already exists')
    binding={'version':1,'unit':unit,'work':str(work),'work_identity':ident(work),
        'lease':str(lease),'lease_identity':lease_identity,
        'command_sha256':hashlib.sha256(json.dumps(command,separators=(',',':')).encode()).hexdigest()}
    new_json(work/'supervisor-start.json',binding)
    child=subprocess.Popen(command,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    observed=None;deadline=time.monotonic()+timeout
    try:
        while child.poll() is None:
            if time.monotonic()>=deadline:
                raise Refuse('supervisor client deadline; service unresolved')
            state=properties(unit)
            if observed is None and state['LoadState']=='loaded' and state['ActiveState']=='active':
                if state['InvocationID'] and state['ControlGroup']=='/system.slice/'+unit:
                    observed=dict(binding,invocation=state['InvocationID'],cgroup=state['ControlGroup'])
                    new_json(work/'supervisor-observed.json',observed)
            time.sleep(.01)
        return child.returncode,observed
    except BaseException:
        # Reap only this owned waiting client. This says nothing about whether
        # systemd's service or descendants stopped; no qualification is returned.
        if child.poll() is None:
            child.terminate()
            try:child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=5)
        else:
            child.wait()
        raise



def qualify_failure(work, lease, consumer, observed, code):
    """Synchronous terminal revalidation under caller-held same consumer lease."""
    work=Path(work)
    if not observed or type(code) is not int or code<=0:raise Refuse('unknown supervised failure')
    opened=os.fstat(consumer.fileno())
    if (observed.get('work')!=str(work) or observed.get('work_identity')!=ident(work)
            or observed.get('lease')!=str(lease) or observed.get('lease_identity')!=ident(lease)
            or [opened.st_dev,opened.st_ino]!=observed.get('lease_identity')):
        raise Refuse('supervisor custody changed')
    saved=json.loads((work/'supervisor-observed.json').read_text())
    start=json.loads((work/'supervisor-start.json').read_text())
    if saved!=observed or start!={k:v for k,v in observed.items() if k not in ('invocation','cgroup')}:
        raise Refuse('supervisor durable binding changed')
    unit=unit_for(work)
    if observed.get('unit')!=unit or observed.get('cgroup')!='/system.slice/'+unit:raise Refuse('supervisor namespace changed')
    before=properties(unit)
    expected={'LoadState':'loaded','ActiveState':'failed','SubState':'failed','InvocationID':observed['invocation'],
        'MainPID':'0','ControlPID':'0','Result':'exit-code','ExecMainStatus':str(code)}
    if any(before.get(k)!=v for k,v in expected.items()):raise Refuse('supervisor outcome unresolved')
    group=Path('/sys/fs/cgroup')/observed['cgroup'].lstrip('/')
    current_group=before['ControlGroup']
    if current_group not in ('',observed['cgroup']):raise Refuse('supervisor cgroup changed')
    if current_group=='' and group.exists():raise Refuse('supervisor cgroup disappearance unresolved')
    if group.exists():
        events=dict(line.split() for line in (group/'cgroup.events').read_text().splitlines())
        if events.get('populated')!='0':raise Refuse('supervisor descendants remain')
    if properties(unit)!=before or ident(lease)!=observed['lease_identity'] or ident(work)!=observed['work_identity']:
        raise Refuse('supervisor changed during qualification')
    return dict(observed,exit_code=code,state='known_failed_unit')
