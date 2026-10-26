import { useEffect, useMemo, useRef } from 'react';
import type { ReviewQueue } from './types';
import type { ReviewFronts } from './review-fronts';
import { reviewQueueFrame } from './review-frame';
import { useRetainedRead } from './retained-read';

type FrameRead<T> = { data: T | undefined; error?: unknown; isFetching: boolean; refetch: (options: { cancelRefetch: boolean }) => Promise<unknown> };

export function useReviewQueueFrame(scope: string, queue: FrameRead<ReviewQueue>, fronts: FrameRead<ReviewFronts>, enabled = true) {
  const candidate = useMemo(() => reviewQueueFrame(queue.data, fronts.data), [queue.data, fronts.data]);
  const frame = useRetainedRead(scope, candidate, enabled && !!queue.data);
  const repair = useRef({ scope: '', snapshot: '', attempts: 0 });
  useEffect(() => {
    if (candidate) { repair.current = { scope, snapshot: candidate.queue.snapshot_id, attempts: 0 }; return; }
    if (!enabled || queue.error || fronts.error || !queue.data?.configured || !fronts.data?.configured || queue.isFetching || fronts.isFetching) return;
    if (repair.current.scope !== scope || repair.current.snapshot !== queue.data.snapshot_id) repair.current = { scope, snapshot: queue.data.snapshot_id, attempts: 0 };
    if (repair.current.attempts >= 2) return;
    // A write can land between the two reads. Reconcile locally without waiting
    // for another user action/event; deduplicate the hero and list's shared reads.
    const timer = setTimeout(() => {
      repair.current.attempts++;
      void Promise.allSettled([queue.refetch({ cancelRefetch: false }), fronts.refetch({ cancelRefetch: false })]);
    }, 100);
    return () => clearTimeout(timer);
  }, [scope, enabled, candidate, queue.data, fronts.data, queue.isFetching, fronts.isFetching, queue.error, fronts.error, queue.refetch, fronts.refetch]);
  // Before any coherent frame has been observed, ordinary source/closure
  // information may render. The row's existing snapshot/source guards still
  // reject mismatched position claims; this fallback is never retained.
  return frame ?? (queue.data ? { queue: queue.data, fronts: fronts.data } : undefined);
}
