import type { QueryClient, QueryKey } from '@tanstack/react-query';
import type { ProjectMetadataPage } from './types';
import { refreshEventQueries } from './queries';

export function isCurrentProjectView(project: string, pathname: string, lifetime: AbortController, current: AbortController | undefined): boolean {
  const path = `/projects/${encodeURIComponent(project)}`;
  return lifetime === current && !lifetime.signal.aborted && (pathname === path || pathname.startsWith(`${path}/`));
}

// Only affected stored-data views are invalidated. The shared helper cancels
// pre-event reads, refetches active queries and leaves unopened bodies alone.
export async function refreshProjectMetadata(client: QueryClient, key: QueryKey, page: ProjectMetadataPage | undefined, signal: AbortSignal): Promise<void> {
  const all = page === undefined;
  const items = page?.items ?? [];
  const prefixes = new Map<string, QueryKey>();
  const add = (...parts: string[]) => { const prefix = [...key, ...parts]; prefixes.set(JSON.stringify(prefix), prefix); };
  if (all) {
    for (const domain of ['delivery', 'map-editorial', 'map', 'map-entry', 'map-history', 'job-workstream', 'job-workstream-history', 'jobs', 'job', 'workers', 'worker', 'worker-pause', 'worker-pause-history', 'worker-primary', 'worker-primary-history', 'notes', 'note', 'note-history', 'checklist-current', 'checklist-history', 'checkpoint-latest', 'checkpoints', 'job-updates']) add(domain);
    add('reviews', 'current'); add('reviews', 'policy-assessment');
  }
  for (const { event_type: type, resource_id: id } of items) {
    if (type === 'coordination.imported') {
      // One batch notification replaces replay of historical operational events.
      // Refresh active imported domains, leaving unopened bodies unfetched.
      for (const domain of ['delivery', 'jobs', 'job', 'checkpoint-latest', 'checkpoints']) add(domain);
    } else if (type.startsWith('map.')) {
      add('map-editorial'); add('map'); add('map-entry', id); add('map-history', id);
      // An entry rename/archive affects recorded references in current views.
      add('job-workstream');
    } else if (type === 'job.created' || type === 'job.updated') {
      add('delivery'); add('jobs'); add('job', id);
      // Captured revision labels must be compared with the new job revision.
      add('checklist-current', id); add('checkpoint-latest', id);
      // Assigned-job note lists depend on the job's current worker.
      add('notes');
    } else if (type === 'job.workstream_changed') {
      add('job-workstream', id); add('job-workstream-history', id);
    } else if (type === 'job.checkpointed') {
      // Append events identify the new record, not its owning job. Refresh only
      // this domain's active views; do not expand every job's private context.
      add('delivery'); add('checkpoint-latest'); add('checkpoints');
    } else if (type === 'job.update_appended') {
      add('job-updates');
    } else if (type === 'job.checklist_replaced') {
      add('checklist-current', id); add('checklist-history', id);
    } else if (type === 'workers.created') {
      add('delivery'); add('workers'); add('worker', id);
    } else if (type === 'worker.pause_changed') {
      add('delivery'); add('worker-pause', id); add('worker-pause-history', id);
    } else if (type === 'worker.primary_job_changed') {
      add('delivery'); add('worker-primary', id); add('worker-primary-history', id);
    } else if (type === 'note.created' || type === 'note.updated') {
      add('notes'); add('note', id); add('note-history', id);
    } else if (type === 'review.requested' || type === 'review.operation_updated') {
      add('reviews', 'current', 'round-progress');
    } else if (type.startsWith('review.') || type.startsWith('reviews.')) {
      add('reviews', 'current'); add('reviews', 'policy-assessment');
    }
  }
  // Prefixes within a page can overlap (for example a map rename and job
  // reference change); visit each active query only once for the batch.
  const selected = [...prefixes.values()].filter(prefix => ![...prefixes.values()].some(other => other.length < prefix.length && other.every((part, index) => part === prefix[index])));
  for (const prefix of selected) { if (signal.aborted) return; await refreshEventQueries(client, prefix, signal); }
}
