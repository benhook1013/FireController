import { useEffect } from 'react';
import { useQuery } from '@tanstack/react-query';
import { signalFor, useWorkspace } from './workspace';
import { roundProgressState, roundProgressAllowance, roundProgressStopReason, type ReviewRoundProgress } from './review-progress';

export function useReviewProgress(pulls: number[]) {
  const { session, key, fail } = useWorkspace();
  // Stable membership shares one read between the hero and queue.
  const selected = [...pulls].sort((left, right) => left - right);
  const query = useQuery({ queryKey: [...key, 'reviews', 'current', 'round-progress', selected],
    queryFn: ({ signal }) => session.api.reviewProgress(selected, signalFor(session, signal)),
    enabled: selected.length > 0, staleTime: Infinity });
  useEffect(() => { if (query.error) fail(query.error); }, [query.error, fail]);
  return query;
}
export function ReviewProgressView({ value }: { value?: ReviewRoundProgress }) {
  if (!value?.configured) return null;
  const stopReason = roundProgressStopReason(value);
  const finished = !!stopReason;
  const remaining = value.maximum_unbounded || value.used < value.maximum;
  return <div className="review-current-progress" aria-label={`Current ${value.channel} round limits`}>
    {finished ? <details><summary className="review-progress-state">{roundProgressState(value)}</summary><span>{stopReason}</span></details> : <span className="review-progress-state">{roundProgressAllowance(value)}</span>}
    {!finished && remaining && value.minimum > value.used && <span>At least {value.minimum - value.used} more</span>}
    {!finished && remaining && <span title={`Consecutive eligible rounds with no accepted findings: ${value.dry}; target ${value.taper}. A maximum can finish early after its minimum is met.`}>{value.dry >= value.taper ? 'Clean streak target met' : `Taper ${value.dry}/${value.taper}`}</span>}
    {value.pending > 0 && <span>{value.pending} in flight</span>}
  </div>;

}
