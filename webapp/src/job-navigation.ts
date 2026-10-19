import { sessionIdentity } from './browser-session';
import type { JobSummary } from './types';

export type JobFilters = { status: JobSummary['status'] | 'open' | ''; workerID: string };
const statuses = new Set(['open', 'all', 'active', 'parked', 'blocked', 'completed']);

// Only these validated selectors travel between the list and a job detail.
// Missing/invalid status means open; explicit all omits the API status filter.
export function readJobFilters(search: URLSearchParams): JobFilters {
  const statusValues = search.getAll('status');
  const status = statusValues.length === 1 && statuses.has(statusValues[0]) ? statusValues[0] : 'open';
  const workers = search.getAll('worker_id');
  return { status: status === 'all' ? '' : status as JobFilters['status'], workerID: workers.length === 1 && sessionIdentity(workers[0]) ? workers[0] : '' };
}

export function jobFilterSearch(filters: JobFilters): string {
  const search = new URLSearchParams();
  if (filters.status === '') search.set('status', 'all');
  else if (filters.status !== 'open' && statuses.has(filters.status)) search.set('status', filters.status);
  if (sessionIdentity(filters.workerID)) search.set('worker_id', filters.workerID);
  return search.size ? `?${search}` : '';
}
