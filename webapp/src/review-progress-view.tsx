import { useQuery } from '@tanstack/react-query';
import { ReviewOperationWaitBadge } from './review-operation-wait';
import { roundProgressAllowance, roundProgressState, roundProgressStopReason, type ReviewRoundProgress } from './review-progress';
import { signalFor, useWorkspace } from './workspace';

export function useReviewProgress(pulls: number[]) {
  const { session, key } = useWorkspace();
  const selected = [...new Set(pulls)].sort((a, b) => a - b);
  const selection = selected.join(',');
  return useQuery({
    queryKey: [...key, 'reviews', 'current', 'round-progress', selection],
    queryFn: ({ signal }) => session.api.reviewProgress(selected, signalFor(session, signal)),
    enabled: selected.length > 0,
    staleTime: Infinity,
  });
}

export function ReviewProgressView({ channel, progress }: { channel: 'hosted' | 'cli'; progress?: ReviewRoundProgress }) {
  if (!progress || !progress.configured && !progress.wait) return null;
  const stopReason = roundProgressStopReason(progress);
  const pending = progress.pending_execution !== undefined
    ? <span>{progress.pending_execution} in execution · {progress.pending_adjudication} awaiting adjudication</span>
    : <span>{progress.pending} pending</span>;
  return <div className="review-current-progress" aria-label={`${channel === 'hosted' ? 'Hosted' : 'CLI'} current round progress`}>
    {progress.configured && <>
      <span className="review-progress-state">{stopReason ? `${roundProgressState(progress)} · ${stopReason}` : roundProgressAllowance(progress)}</span>
      <span>{progress.used} used</span>
      {!stopReason && progress.minimum > progress.used && <span>At least {progress.minimum - progress.used} more</span>}
      {!stopReason && <span>Taper {progress.dry}/{progress.taper}</span>}
    </>}
    {progress.configured && pending}
    {progress.wait && <ReviewOperationWaitBadge wait={progress.wait} channel={channel} />}
  </div>;
}
