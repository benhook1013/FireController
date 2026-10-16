import type { QueryClient, QueryKey } from '@tanstack/react-query';
import type { ProjectMetadataPage } from './types';
import { refreshEventQueries } from './queries';

export function isCurrentProjectView(project: string, pathname: string, lifetime: AbortController, current: AbortController | undefined): boolean {
  const path = `/projects/${encodeURIComponent(project)}`;
  return lifetime === current && !lifetime.signal.aborted && (pathname === path || pathname.startsWith(`${path}/`));
}

// Invalidation stays scoped to stored map/reference views. Inactive pages are
// marked stale by the shared helper without being fetched or walked.
export async function refreshProjectMetadata(client: QueryClient, key: QueryKey, page: ProjectMetadataPage | undefined, signal: AbortSignal): Promise<void> {
  const all = page === undefined;
  const entries = new Set(page?.items.filter(item => item.event_type.startsWith('map.')).map(item => item.resource_id));
  const jobs = new Set(page?.items.filter(item => item.event_type === 'job.workstream_changed').map(item => item.resource_id));
  const prefixes: QueryKey[] = [];
  if (all || entries.size > 0) {
    prefixes.push([...key, 'map'], [...key, 'job-workstream']);
    if (all) prefixes.push([...key, 'map-entry'], [...key, 'map-history']);
    else for (const id of entries) prefixes.push([...key, 'map-entry', id], [...key, 'map-history', id]);
  } else for (const id of jobs) prefixes.push([...key, 'job-workstream', id]);
  // Sequential bounded prefixes avoid concurrent duplicate refetches of a view.
  for (const prefix of prefixes) { if (signal.aborted) return; await refreshEventQueries(client, prefix, signal); }
}
