import { useEffect } from 'react';
import { useQuery } from '@tanstack/react-query';
import { signalFor, useWorkspace } from './workspace';
import { roundProgressState, type ReviewRoundProgress } from './review-progress';

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
  return <div className="review-current-progress" aria-label={`Current ${value.channel} round limits`}>
    <span className="review-progress-state">{roundProgressState(value)}</span>
    <span>{value.used} of {value.maximum} used · Max {value.maximum - value.used} remaining</span>
    {value.minimum > value.used && <span>Minimum {value.minimum - value.used} more</span>}
    <span title="Consecutive eligible rounds with no accepted findings; a maximum can finish early after its minimum is met.">Taper {value.dry}/{value.taper}</span>
    {value.pending > 0 && <span>{value.pending} in flight</span>}
  </div>;
}
