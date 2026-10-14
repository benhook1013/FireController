import type { QueryClient, QueryKey } from '@tanstack/react-query';

// Require a post-event read before the caller advances its processed cursor.
// Invalidation alone can reuse a pending first read with a pre-event snapshot.
export async function refreshEventQueries(client: QueryClient, queryKey: QueryKey, signal: AbortSignal, skipFirstPage = false): Promise<void> {
  if (signal.aborted) return;
  const predicate = (query: { queryKey: QueryKey }) => !skipFirstPage || query.queryKey.at(-1) !== '';
  await client.cancelQueries({ queryKey, predicate, type: 'active' });
  if (signal.aborted) return;
  // Inactive pages must also become stale for their next explicit visit.
  await client.invalidateQueries({ queryKey, predicate, refetchType: 'active' }, { throwOnError: true });
}
