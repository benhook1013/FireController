import type { QueryClient, QueryKey } from '@tanstack/react-query';
import { ApiError } from './api';

// Require a post-event read before the caller advances its processed cursor.
// Invalidation alone can reuse a pending first read with a pre-event snapshot.
export async function refreshEventQueries(client: QueryClient, queryKey: QueryKey, signal: AbortSignal, skipFirstPage = false): Promise<void> {
  if (signal.aborted) return;
  const predicate = (query: { queryKey: QueryKey }) => !skipFirstPage || query.queryKey.at(-1) !== '';
  await client.cancelQueries({ queryKey, predicate, type: 'active' });
  if (signal.aborted) return;
  // Inactive pages must also become stale for their next explicit visit.
  await client.invalidateQueries({ queryKey, predicate, refetchType: 'none' });
  if (signal.aborted) return;
  // Observe every result: an unavailable view must not hide another query's
  // authorization failure or prevent healthy views from completing their read.
  const results = await Promise.allSettled(client.getQueryCache().findAll({ queryKey, predicate, type: 'active' }).map(query =>
    client.refetchQueries({ queryKey: query.queryKey, exact: true, type: 'active' }, { throwOnError: true }),
  ));
  const failures = results.filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    .map(result => result.reason as unknown)
    .filter(error => !(error instanceof ApiError && error.code === 'not_found' && error.status === 404));
  const failure = failures.find(error => error instanceof ApiError && (error.status === 401 || error.status === 403 || ['unauthorized', 'forbidden', 'instance_mismatch', 'invalid_binding'].includes(error.code)))
    ?? failures.find(error => error instanceof ApiError && error.code === 'invalid_response')
    ?? failures[0];
  if (failures.length > 0) throw failure;
}
