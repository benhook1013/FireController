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
  it('refreshes imported job views without replaying messages or fetching unopened bodies', async () => {
    const f = fixture();
    const jobs = f.add(['jobs', '', false, '']);
    const checkpoint = f.add(['checkpoint-latest', 'job_one']);
    const inactive = f.add(['job', 'not_open'], false);
    const inbox = f.add(['inbox', '']);
    const reviews = f.add(['reviews', 'current', 'queue', '']);
    try {
      await refreshProjectMetadata(f.client, key, page([{ event_type: 'coordination.imported', resource_id: 'project' }]), new AbortController().signal);
      expect(jobs.reads()).toBe(1); expect(checkpoint.reads()).toBe(1);
      expect(inactive.reads()).toBe(0); expect(inactive.state()?.isInvalidated).toBe(true);
      expect(inbox.reads()).toBe(0); expect(reviews.reads()).toBe(0);
    } finally { f.close(); }
  });
  it('coalesces job changes into affected active reads without fetching unrelated context or archives', async () => {
    const f = fixture();
    const jobs = f.add(['jobs', '', false, '']);
    const job = f.add(['job', 'job_one']);
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
      for (const query of [jobs, job, checklist, checkpoint, updates, notes]) expect(query.reads()).toBe(1);
      expect(closedArchive.reads()).toBe(0); expect(closedArchive.state()?.isInvalidated).toBe(true);
      expect(inactiveNotes.reads()).toBe(0); expect(inactiveNotes.state()?.isInvalidated).toBe(true);
      for (const query of [otherJob, inbox, otherProject]) expect(query.reads()).toBe(0);
    } finally { f.close(); }
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
    try {
      await refreshProjectMetadata(f.client, key, undefined, new AbortController().signal);
      for (const query of [...active, reviews]) expect(query.reads()).toBe(1);
      expect(inactiveBody.reads()).toBe(0); expect(inactiveBody.state()?.isInvalidated).toBe(true);
      for (const query of [immutable, inbox]) expect(query.reads()).toBe(0);
      const canceled = new AbortController(); canceled.abort();
      await refreshProjectMetadata(f.client, key, undefined, canceled.signal);
      for (const query of [...active, reviews]) expect(query.reads()).toBe(1);
    } finally { f.close(); }
  });

  it('accepts only the bounded explicit change vocabulary without messages or payloads', () => {
    const complete = page(projectEventTypes.map(event_type => ({ event_type, resource_id: 'record_one' })));
    expect(validMetadataPage(complete, 100)).toBe(true);
    for (const kind of ['message.sent', 'credentials.revoked', 'review.cli_preflight', 'review.future_kind']) {
      expect(validMetadataPage({ ...complete, items: [{ ...complete.items[0], event_type: kind }] }, 25)).toBe(false);
    }
    expect(validMetadataPage({ ...complete, items: [{ ...complete.items[0], payload: { body: 'private' } }] }, 25)).toBe(false);
  });
});
