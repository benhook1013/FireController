import { QueryClient, QueryObserver, type QueryKey } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { projectEventTypes } from './project-event-types';
import { refreshProjectMetadata } from './project-metadata';
import { validMetadataPage } from './presentation';
import type { ProjectMetadataEvent, ProjectMetadataPage } from './types';

const key = ['fc', 'session', 'instance', 'project', 'worker'];
const page = (items: Pick<ProjectMetadataEvent, 'event_type' | 'resource_id'>[]): ProjectMetadataPage => ({
  items: items.map((item, i) => ({ ...item, revision: 2, event_cursor: `cursor_${i}` })),
  event_cursor: 'end_cursor', has_more: false,
});
function fixture() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
  const counts = new Map<string, number>(), subscriptions: (() => void)[] = [];
  const add = (suffix: QueryKey, active = true, prefix: QueryKey = key) => {
    const queryKey = [...prefix, ...suffix], id = JSON.stringify(queryKey);
    client.setQueryData(queryKey, 'old'); counts.set(id, 0);
    if (active) subscriptions.push(new QueryObserver(client, { queryKey, queryFn: async () => {
      counts.set(id, counts.get(id)! + 1); return 'committed';
    } }).subscribe(() => {}));
    return { reads: () => counts.get(id), state: () => client.getQueryState(queryKey) };
  };
  return { client, add, close: () => { subscriptions.forEach(stop => stop()); client.clear(); } };
}

describe('live private project views', () => {
  it('refreshes selected job summaries and delivery without touching private briefs, archives or another project', async () => {
    const f=fixture(), summary=f.add(['job-summary','job_one']), delivery=f.add(['delivery','','']), brief=f.add(['job','job_one']), other=f.add(['job-summary','job_one'],true,['fc','session','instance','other','worker']);
    try {
      await refreshProjectMetadata(f.client,key,page([{event_type:'job.summary_replaced',resource_id:'job_one'}]),new AbortController().signal);
      expect(summary.reads()).toBe(1);expect(delivery.reads()).toBe(1);expect(brief.reads()).toBe(0);expect(other.reads()).toBe(0);
    } finally {f.close();}
  });

  it('updates current round occupancy and fronts without refetching queue or pinned review history', async () => {
    const f=fixture();const progress=f.add(['reviews','current','round-progress',[31,32]]);const fronts=f.add(['reviews','current','fronts']);
    const queue=f.add(['reviews','current','queue','']);const history=f.add(['reviews','snapshot_old','run','run_one']);const inbox=f.add(['inbox','']);
    try { await refreshProjectMetadata(f.client,key,page([{event_type:'review.requested',resource_id:'operation_one'},{event_type:'review.operation_updated',resource_id:'operation_two'}]),new AbortController().signal);
      expect(progress.reads()).toBe(1);expect(fronts.reads()).toBe(1);for(const query of [queue,history,inbox])expect(query.reads()).toBe(0);
    } finally {f.close();}
  });

  it('refreshes active job and message views once for an import without opening bodies or crossing identities', async () => {
    const f = fixture();
    const jobs = f.add(['jobs', '', false, '']);
    const checkpoint = f.add(['checkpoint-latest', 'job_one']);
    const inactive = f.add(['job', 'not_open'], false);
    const inbox = f.add(['inbox', '']);
    const thread = f.add(['thread', 'conversation_one', '']);
    const unopenedThread = f.add(['thread', 'not_open', ''], false);
    const unopenedInboxPage = f.add(['inbox', 'older_cursor'], false);
    const reviews = f.add(['reviews', 'current', 'queue', '']);
    const captured = f.add(['reviews', 'snapshot_old', 'run', 'run_one']);
    const otherIdentities = [
      ['fc', 'other_session', 'instance', 'project', 'worker'],
      ['fc', 'session', 'other_instance', 'project', 'worker'],
      ['fc', 'session', 'instance', 'other_project', 'worker'],
      ['fc', 'session', 'instance', 'project', 'other_worker'],
    ].flatMap(prefix => [f.add(['inbox', ''], true, prefix), f.add(['thread', 'conversation_one', ''], true, prefix)]);
    try {
      // A message-only import emits this batch hint, not historical message events.
      await refreshProjectMetadata(f.client, key, page([
        { event_type: 'coordination.imported', resource_id: 'project' },
        { event_type: 'coordination.imported', resource_id: 'project' },
      ]), new AbortController().signal);
      for (const query of [jobs, checkpoint, inbox, thread]) {
        expect(query.reads()).toBe(1); expect(query.state()?.data).toBe('committed');
      }
      for (const query of [inactive, unopenedThread, unopenedInboxPage]) {
        expect(query.reads()).toBe(0); expect(query.state()?.isInvalidated).toBe(true); expect(query.state()?.data).toBe('old');
      }
      for (const query of [reviews, captured, ...otherIdentities]) {
        expect(query.reads()).toBe(0); expect(query.state()?.isInvalidated).toBe(false); expect(query.state()?.data).toBe('old');
      }
      expect(f.client.getMutationCache().getAll()).toHaveLength(0);
    } finally { f.close(); }
  });
  it('refreshes unified current progress and invalidates only its explicit history on job changes', async () => {
    const f=fixture(), job=f.add(['job','job_one']), history=f.add(['job-progress-history','job_one',''],false), openHistory=f.add(['job-progress-history','job_one','open']), other=f.add(['job','job_two']), inbox=f.add(['inbox','']), old=f.add(['job-progress-history','job_one',''],false,['fc','old','instance','project','worker']);
    try {
      await refreshProjectMetadata(f.client,key,page([{event_type:'job.updated',resource_id:'job_one'}]),new AbortController().signal);
      expect(job.reads()).toBe(1); expect(openHistory.reads()).toBe(1); expect(history.reads()).toBe(0); expect(history.state()?.isInvalidated).toBe(true);
      for(const query of [other,inbox,old]) { expect(query.reads()).toBe(0); expect(query.state()?.isInvalidated).toBe(false); }
    } finally {f.close();}
  });
  it('refreshes default attached notes through active jobs without opening histories or worker inboxes', async () => {
    const f=fixture(), job=f.add(['job','job_one']), history=f.add(['job-progress-history','job_one',''],false), inbox=f.add(['inbox','']), other=f.add(['job','job_one'],true,['fc','session','instance','foreign','worker']);
    try {
      await refreshProjectMetadata(f.client,key,page([{event_type:'note.updated',resource_id:'note_one'}]),new AbortController().signal);
      expect(job.reads()).toBe(1);
      for(const query of [history,inbox,other]) {expect(query.reads()).toBe(0);expect(query.state()?.isInvalidated).toBe(false);}
    } finally {f.close();}
  });
  it('coalesces job changes into affected active reads without fetching unrelated context or archives', async () => {
    const f = fixture();
    const jobs = f.add(['jobs', '', false, '']);
    const job = f.add(['job', 'job_one']);
    const summary = f.add(['job-summary', 'job_one']);
    const checklist = f.add(['checklist-current', 'job_one']);
    const checkpoint = f.add(['checkpoint-latest', 'job_one']);
    const updates = f.add(['job-updates', 'job_one', '']);
    const closedArchive = f.add(['checkpoints', 'job_one', ''], false);
    const otherJob = f.add(['job', 'other_job']);
    const notes = f.add(['notes', { worker_id: 'worker_one', include_assigned_jobs: true }, '']);
    const inactiveNotes = f.add(['notes', { worker_id: 'worker_two', include_assigned_jobs: true }, ''], false);
    const inbox = f.add(['inbox', '']);
    const otherProject = f.add(['jobs', '', false, ''], true, ['fc', 'session', 'instance', 'other', 'worker']);
    try {
      await refreshProjectMetadata(f.client, key, page([
        { event_type: 'job.updated', resource_id: 'job_one' },
        { event_type: 'job.updated', resource_id: 'job_one' },
        { event_type: 'job.checkpointed', resource_id: 'checkpoint_record' },
        { event_type: 'job.checklist_replaced', resource_id: 'job_one' },
        { event_type: 'job.update_appended', resource_id: 'update_record' },
      ]), new AbortController().signal);
      for (const query of [jobs, job, summary, checklist, checkpoint, updates, notes]) expect(query.reads()).toBe(1);
      expect(closedArchive.reads()).toBe(0); expect(closedArchive.state()?.isInvalidated).toBe(true);
      expect(inactiveNotes.reads()).toBe(0); expect(inactiveNotes.state()?.isInvalidated).toBe(true);
      for (const query of [otherJob, inbox, otherProject]) expect(query.reads()).toBe(0);
    } finally { f.close(); }
  });

  it.each(['job.update_appended','job.checklist_replaced'] as const)('refreshes Delivery and any identified current job on %s without archive fanout',async event_type=>{
    const f=fixture(), delivery=f.add(['delivery','','']), job=f.add(['job','job_one']), archive=f.add(['checkpoints','job_one',''],false);
    try {
      await refreshProjectMetadata(f.client,key,page([{event_type,resource_id:event_type === 'job.checklist_replaced' ? 'job_one' : 'record_one'}]),new AbortController().signal);
      expect(delivery.reads()).toBe(1); expect(job.reads()).toBe(event_type === 'job.checklist_replaced' ? 1 : 0); expect(archive.reads()).toBe(0); expect(archive.state()?.isInvalidated).toBe(false);
    } finally {f.close();}
  });

  it('updates worker, note and current review views while preserving captured history and private inbox scope', async () => {
    const f = fixture();
    const workers = f.add(['workers', '']);
    const pause = f.add(['worker-pause', 'worker_one']);
    const focus = f.add(['worker-primary', 'worker_one']);
    const notes = f.add(['notes', { status: 'pending' }, '']);
    const note = f.add(['note', 'note_one']);
    const current = f.add(['reviews', 'current', 'queue', '']);
    const assessment = f.add(['reviews', 'policy-assessment', 'evidence_one']);
    const captured = f.add(['reviews', 'snapshot_old', 'run', 'run_one']);
    const inbox = f.add(['inbox', '']);
    try {
      await refreshProjectMetadata(f.client, key, page([
        { event_type: 'workers.created', resource_id: 'worker_two' },
        { event_type: 'worker.pause_changed', resource_id: 'worker_one' },
        { event_type: 'worker.primary_job_changed', resource_id: 'worker_one' },
        { event_type: 'note.updated', resource_id: 'note_one' },
        { event_type: 'review.observed', resource_id: 'snapshot_new' },
        { event_type: 'reviews.policy_configured', resource_id: 'project' },
        { event_type: 'reviews.adoption_obligations_imported', resource_id: 'sealed-source' },
        { event_type: 'reviews.adoption_obligation_resolved', resource_id: 'adoption_obligation_one' },
        { event_type: 'review.evidence.finalized', resource_id: 'evidence_one' },
      ]), new AbortController().signal);
      for (const query of [workers, pause, focus, notes, note, current, assessment]) expect(query.reads()).toBe(1);
      for (const query of [captured, inbox]) { expect(query.reads()).toBe(0); expect(query.state()?.isInvalidated).toBe(false); }
    } finally { f.close(); }
  });

  it('resnapshots all mutable domains without opening inactive private bodies or rereading immutable revisions', async () => {
    const f = fixture();
    const active = ['jobs', 'workers', 'notes', 'map'].map(domain => f.add([domain, '']));
    const reviews = f.add(['reviews', 'current', 'queue', '']);
    const inactiveBody = f.add(['note', 'not_open'], false);
    const immutable = f.add(['map-revision', 'entry_one', 1]);
    const inbox = f.add(['inbox', '']);
    const thread = f.add(['thread', 'conversation_one', '']);
    const unopenedThread = f.add(['thread', 'not_open', ''], false);
    const unopenedInboxPage = f.add(['inbox', 'older_cursor'], false);
    const captured = f.add(['reviews', 'snapshot_old', 'run', 'run_one']);
    const otherIdentities = [
      ['fc', 'other_session', 'instance', 'project', 'worker'],
      ['fc', 'session', 'other_instance', 'project', 'worker'],
      ['fc', 'session', 'instance', 'other_project', 'worker'],
      ['fc', 'session', 'instance', 'project', 'other_worker'],
    ].flatMap(prefix => [f.add(['inbox', ''], true, prefix), f.add(['thread', 'conversation_one', ''], true, prefix)]);
    try {
      await refreshProjectMetadata(f.client, key, undefined, new AbortController().signal);
      for (const query of [...active, reviews, inbox, thread]) {
        expect(query.reads()).toBe(1); expect(query.state()?.data).toBe('committed');
      }
      for (const query of [inactiveBody, unopenedThread, unopenedInboxPage]) {
        expect(query.reads()).toBe(0); expect(query.state()?.isInvalidated).toBe(true); expect(query.state()?.data).toBe('old');
      }
      for (const query of [immutable, captured, ...otherIdentities]) {
        expect(query.reads()).toBe(0); expect(query.state()?.isInvalidated).toBe(false); expect(query.state()?.data).toBe('old');
      }
      expect(f.client.getMutationCache().getAll()).toHaveLength(0);
      const canceled = new AbortController(); canceled.abort();
      await refreshProjectMetadata(f.client, key, undefined, canceled.signal);
      for (const query of [...active, reviews, inbox, thread]) expect(query.reads()).toBe(1);
    } finally { f.close(); }
  });

  it('accepts only the bounded explicit change vocabulary without messages or payloads', () => {
    const complete = page(projectEventTypes.map(event_type => ({ event_type, resource_id: event_type === 'reviews.rounds_configured' ? '32/cli' : 'record_one' })));
    expect(validMetadataPage(complete, 100)).toBe(true);
    for (const kind of ['message.sent', 'credentials.revoked', 'review.cli_preflight', 'review.future_kind']) {
      expect(validMetadataPage({ ...complete, items: [{ ...complete.items[0], event_type: kind }] }, 25)).toBe(false);
    }
    expect(validMetadataPage({ ...complete, items: [{ ...complete.items[0], payload: { body: 'private' } }] }, 25)).toBe(false);
  });
});
