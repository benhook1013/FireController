#!/usr/bin/env python3
# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Explicit source-owner frame acquisition; never run on ambient state.

The source owner supplies a sealed canonical-layout handoff and confirms writer
pause. Git proof writes are confined to a separately owned disposable clone.
This is optional migration glue, not a native service dependency.
"""
from contextlib import ExitStack
import importlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import time

from source_acquisition_frame import acquire_frame, FrameRefusal
from source_acquisition_conservation import conserved_roots, ConservationRefusal
from source_acquisition_bindings import bind_source, exact_path, BindingRefusal

ADAPTER = Path(__file__).with_name('prepare_source_controls.py')
spec = importlib.util.spec_from_file_location('source_transfer_adapter', ADAPTER)
transfer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(transfer)


class AcquisitionRefusal(Exception):
    pass


FRAME_REFUSAL_ACTIONS = {
    'source_writers_not_paused': 'Pause source writers before acquiring a fresh sealed handoff.',
    'source_binding_invalid': 'Verify the repository identity and sealed snapshot digest.',
    'source_selection_invalid': 'Select valid distinct PRs and channels in the acquisition plan.',
    'fixture_clearance_forbidden': 'Select the actual trusted source controller, not a fixture.',
    'source_selection_not_configured': 'Select PRs present in the source review queue.',
    'source_cli_lock_uncertain': 'Verify the source CLI lock is clear before a fresh acquisition.',
    'source_requests_not_clear': 'Resolve retained source requests and verify they are clear before a fresh acquisition.',
    'source_routes_not_clear': 'Resolve source finding routes before a fresh acquisition.',
    'source_anchor_unavailable': 'Reconcile the source comparison before a fresh acquisition.',
    'source_state_changed': 'Pause source writers and acquire a fresh sealed handoff.',
    'source_frame_not_serializable': 'Inspect the trusted source adapter for unsupported frame values.',
    'source_frame_too_large': 'Select fewer PRs or channels for this acquisition.',
}


def physical_git_metadata(root):
    """Refuse redirected write destinations in an exclusively owned clone."""
    pending, count, deadline=[root], 0, time.monotonic()+10
    while pending:
        path=pending.pop()
        count+=1
        if count>200000 or time.monotonic()>deadline:
            raise AcquisitionRefusal('scratch_git_metadata_too_large')
        info=path.lstat()
        if info.st_uid!=os.getuid():
            raise AcquisitionRefusal('scratch_git_not_isolated')
        if stat.S_ISDIR(info.st_mode):
            with os.scandir(path) as entries:
                for entry in entries:
                    pending.append(path/entry.name)
                    if count+len(pending)>200000:
                        raise AcquisitionRefusal('scratch_git_metadata_too_large')
        elif not stat.S_ISREG(info.st_mode) or info.st_nlink!=1:
            # Symlinked objects/refs or a hardlinked mutable metadata file can
            # write outside the owned clone even when rev-parse looks correct.
            raise AcquisitionRefusal('scratch_git_not_isolated')


def plan_input(value):
    keys={'version','repository','repository_id','default_base_ref','paused',
          'scratch_git_writes','prs'}
    if (type(value) is not dict or set(value)!=keys
            or type(value['version']) is not int or value['version']!=1
            or type(value['repository_id']) is not int
            or not 0 < value['repository_id'] < 2**63
            or value['paused'] is not True or value['scratch_git_writes'] is not True
            or not isinstance(value['repository'],str)
            or not isinstance(value['default_base_ref'],str)
            or not value['default_base_ref'] or len(value['default_base_ref'].encode())>200
            or any(ord(c)<32 for c in value['default_base_ref'])
            or type(value['prs']) is not list or not 1<=len(value['prs'])<=100):
        raise AcquisitionRefusal('acquisition_plan_invalid')
    selected={}
    for row in value['prs']:
        if (type(row) is not dict or set(row)!={'pull_number','channels'}
                or type(row['pull_number']) is not int or not 0<row['pull_number']<2**31
                or row['pull_number'] in selected or type(row['channels']) is not list
                or not row['channels'] or any(type(c) is not str or c not in ('cli','hosted') for c in row['channels'])
                or len(row['channels'])!=len(set(row['channels']))):
            raise AcquisitionRefusal('acquisition_plan_invalid')
        selected[row['pull_number']]=tuple(row['channels'])
    return selected


def scratch_git(root, *, common, source, repository):
    root=exact_path(root)
    if (not root.is_dir() or root.stat().st_mode & 0o077
            or root.stat().st_uid!=os.getuid()
            or any(root.is_relative_to(other) or other.is_relative_to(root)
                   for other in (common, source))
            or any(name.startswith('GIT_') and name not in
                   {'GIT_PAGER','GIT_TERMINAL_PROMPT','GIT_OPTIONAL_LOCKS'}
                   for name in os.environ)):
        raise AcquisitionRefusal('scratch_git_not_isolated')
    gitdir=root/'.git'
    if (gitdir.is_symlink() or not gitdir.is_dir()
            or os.path.lexists(gitdir/'commondir')
            or os.path.lexists(gitdir/'objects/info/alternates')):
        raise AcquisitionRefusal('scratch_git_not_isolated')
    physical_git_metadata(gitdir)
    def git(*args):
        return subprocess.run(['git','-C',str(root),*args],check=True,
            capture_output=True,text=True,timeout=10).stdout.strip()
    if Path(git('rev-parse','--path-format=absolute','--git-common-dir')).resolve()!=gitdir:
        raise AcquisitionRefusal('scratch_git_not_isolated')
    if Path(git('rev-parse','--show-toplevel')).resolve()!=root:
        raise AcquisitionRefusal('scratch_git_not_isolated')
    remote=git('remote','get-url','origin')
    if remote.casefold() not in {f'https://github.com/{repository}'.casefold(),
            f'https://github.com/{repository}.git'.casefold(),
            f'git@github.com:{repository}.git'.casefold()}:
        raise AcquisitionRefusal('scratch_git_repository_mismatch')
    return root


def repository_matches(helpers, plan):
    owner, name=helpers['github'].parse_repo(plan['repository'])
    query='query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){databaseId nameWithOwner defaultBranchRef{name}}}'
    response=helpers['github'].run_gh_query(query,{'owner':owner,'repo':name})
    repo=response['data']['repository']
    if (type(repo.get('databaseId')) is not int or repo['databaseId']!=plan['repository_id']
            or repo.get('nameWithOwner','').casefold()!=plan['repository'].casefold()
            or repo.get('defaultBranchRef',{}).get('name')!=plan['default_base_ref']):
        raise AcquisitionRefusal('source_repository_mismatch')


def construct(helpers, plan, store, records, gitroot):
    live=helpers['runtime'].LiveGitHub(plan['repository'])
    evidence=helpers['runtime'].LiveEvidence(plan['repository'],live,store,records)
    return helpers['controller'].ReviewController(
        store=store,github=live,git=helpers['controller'].DefaultGitProvider(gitroot),
        evidence=evidence,repository=plan['repository'],
        default_base_ref=plan['default_base_ref'],isolated_fixture=False)


def acquire(args):
    source, common=exact_path(args.source_root), exact_path(args.common)
    snapshot=exact_path(args.snapshot)
    output, provenance=exact_path(args.output),exact_path(args.provenance)
    request={name:str(exact_path(getattr(args,name))) for name in
             ('source_root','common','snapshot','git_root','plan','output','provenance')}
    if (output==provenance or any(path in (snapshot,Path(args.plan))
            or path.is_relative_to(common) or path.is_relative_to(source)
            for path in (output,provenance))):
        raise AcquisitionRefusal('output_scope_invalid')
    for path in (output,provenance):
        parent=path.parent.stat()
        if not stat.S_ISDIR(parent.st_mode) or parent.st_mode & 0o077 or parent.st_uid!=os.getuid():
            raise AcquisitionRefusal('output_scope_invalid')
    helpers, hashes, code_hash=transfer.load_helpers(str(source))
    for name in ('runtime','hosted','evidence','sqlite_review_records','github'):
        module=importlib.import_module('pr_review.'+name)
        if not Path(module.__file__).resolve().is_relative_to(source/'dev-tools/pr_review'):
            raise AcquisitionRefusal('source_module_mismatch')
        helpers[name]=module
    with ExitStack() as stack:
        _, plan_bytes=stack.enter_context(transfer.private_input(args.plan,16<<10))
        plan=transfer.strict_json(plan_bytes)
        selected=plan_input(plan)
        helpers['github'].parse_repo(plan['repository'])
        _, snapshot_bytes=stack.enter_context(transfer.private_input(str(snapshot),32<<20,snapshot=True))
        snapshot_sha=transfer.digest(snapshot_bytes)
        roots=(common/'firemud',common/'coderabbit-review-logs')
        capture_inventory=stack.enter_context(conserved_roots(roots))
        inventory_sha=transfer.digest(transfer.canonical(capture_inventory))
        expected=dict(snapshot_sha256=snapshot_sha,source_code_sha256=code_hash,
                      capture_inventory_sha256=inventory_sha,
                      plan_sha256=transfer.digest(plan_bytes))
        replayed=os.path.lexists(provenance)
        if replayed:
            _, proof_bytes=stack.enter_context(transfer.private_input(str(provenance),2<<20))
            proof=transfer.strict_json(proof_bytes)
            if (type(proof) is not dict or proof.get('version')!=1
                    or proof.get('kind')!='source_owner_frame_acquisition'
                    or proof.get('request')!=request or proof.get('source_helpers')!=hashes
                    or type(proof.get('binding')) is not dict
                    or any(proof['binding'].get(k)!=v for k,v in expected.items())
                    or proof.get('source_writer_pause_asserted') is not True
                    or proof.get('durable_writer_fence_established') is not False
                    or proof.get('source_obligations_audited') is not True
                    or proof.get('scratch_git_writes_permitted') is not True):
                raise AcquisitionRefusal('acquisition_replay_conflict')
            frame=proof.get('retained_frame')
            if transfer.digest(transfer.canonical(frame))!=proof['binding'].get('frame_sha256'):
                raise AcquisitionRefusal('acquisition_replay_invalid')
            for pr,channels in selected.items():
                for channel in channels:
                    transfer.selected_history(frame,dict(repository_id=plan['repository_id'],
                        pull_number=pr,channel=channel),snapshot_sha)
        else:
            store, records=stack.enter_context(bind_source(helpers,common=common,snapshot=snapshot))
            gitroot=scratch_git(args.git_root,common=common,source=source,repository=plan['repository'])
            with helpers['github'].activate_hosted_preflight_budget(120,preflight_name='Source export'):
                repository_matches(helpers,plan)
                controller=construct(helpers,plan,store,records,gitroot)
                frame=acquire_frame(controller,records,helpers['policy'],
                    repository_id=plan['repository_id'],snapshot_sha256=snapshot_sha,
                    selected=selected,writer_pause_confirmed=plan['paused'],
                    history_validator=transfer.selected_history)
            proof=dict(version=1,kind='source_owner_frame_acquisition',
                request=request,source_helpers=hashes,retained_frame=frame,
                source_writer_pause_asserted=True,durable_writer_fence_established=False,
                source_obligations_audited=True,scratch_git_writes_permitted=True,
                binding=dict(expected,frame_sha256=transfer.digest(transfer.canonical(frame))))
    # Every conservation context closes before any output is published.
    if transfer.source_hash(str(source))[2]!=code_hash:
        raise AcquisitionRefusal('source_helpers_changed')
    body, proof_body=transfer.canonical(frame),transfer.canonical(proof)
    transfer.check_output(str(output),body)
    transfer.check_output(str(provenance),proof_body)
    transfer.publish(str(provenance),proof_body)
    outcome=transfer.publish(str(output),body)
    return dict(kind='source_owner_frame',outcome=outcome,prs=len(selected),
                channels=sum(len(x) for x in selected.values()),
                sha256=proof['binding']['frame_sha256'],replayed=replayed)


def main(argv=None):
    parser=transfer.QuietParser(allow_abbrev=False,description=__doc__)
    for name in ('source-root','common','snapshot','git-root','plan','output','provenance'):
        parser.add_argument('--'+name,required=True,action=transfer.UniqueInput)
    args=parser.parse_args(argv)
    try:
        result=acquire(args)
    except Exception as exc:
        # Only exact local refusal classes carry fixed diagnostic codes. Never
        # format provider/helper errors or publish source-dependent details.
        code='source_acquisition_refused'
        if type(exc) in (AcquisitionRefusal,transfer.Refusal,BindingRefusal,FrameRefusal,ConservationRefusal):
            value=exc.code if type(exc) is transfer.Refusal else exc.args[0] if len(exc.args)==1 else None
            if type(value) is str and re.fullmatch(r'[a-z][a-z0-9_]{0,63}',value):
                code=value
        report=dict(error=code)
        if type(exc) is FrameRefusal and code in FRAME_REFUSAL_ACTIONS:
            report['next_action']=FRAME_REFUSAL_ACTIONS[code]
        print(json.dumps(report,sort_keys=True,separators=(',',':')),file=sys.stderr)
        return 3
    try:
        print(json.dumps(result,sort_keys=True))
    except OSError:
        print('{"error":"output_report_failed"}',file=sys.stderr)
        return 3
    return 0


if __name__=='__main__':
    raise SystemExit(main())
