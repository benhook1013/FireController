import type { ReviewAttemptSummary } from './types';
import type { ReviewRoundProgress } from './review-progress';
import type { ReviewChannelPosition } from './review-fronts';

export interface ReviewAttemptStatus {
  text: string;
  description?: string;
  kind?: 'attention' | 'history';
}

// An attempt is an execution record, not a verdict on the PR. Only retained
// pending work establishes recovery; queue eligibility alone never does.
export function reviewAttemptStatus(attempt?: ReviewAttemptSummary, progress?: ReviewRoundProgress, position?: ReviewChannelPosition, historical = false): ReviewAttemptStatus | undefined {
  const diagnostic = attempt ? `Retained attempt: ${attempt.state}${attempt.reason ? ` (${attempt.reason})` : ''}. See Attempts in review details.` : '';
  const stopped = attempt && ['failed', 'held', 'rate_limited'].includes(attempt.state);
  if (historical) {
    if (!attempt || attempt.state === 'completed') return undefined;
    return { text: 'Past attempt', kind: 'history', description: `${diagnostic} ${attempt.state === 'uncertain' ? 'The provider outcome remains unconfirmed; PR closure does not reconcile it. ' : ''}Historical record, not current activity.` };
  }
  // An unconfirmed provider outcome must be reconciled, even when another
  // request or a closed grant exists. Do not present it as safe to retry.
  if (attempt?.state === 'uncertain') return { text: 'Needs agent attention', kind: 'attention', description: `The provider outcome is unconfirmed. Reconcile it before retrying. ${diagnostic}` };
  if (progress?.wait) return undefined; // Render the actual retained wait badge.
  if (attempt && !attempt.finished_at && ['preparing', 'prepared', 'posting', 'active'].includes(attempt.state)) {
    return { text: attempt.state === 'active' ? 'Reviewing' : 'Preparing' };
  }
  if ((progress?.pending_execution ?? 0) > 0) return {
    text: stopped ? 'Retry pending' : 'Pending',
    description: stopped ? `A review request is pending after the stopped attempt. This does not establish that a provider review has started. ${diagnostic}` : undefined,
  };
  if ((progress?.pending_adjudication ?? 0) > 0) return { text: 'Needs adjudication' };
  // Older servers expose only a combined pending count. It cannot prove a
  // retry, a running provider, or that the previous failure was resolved.
  if ((progress?.pending ?? 0) > 0) return { text: 'Pending', description: `Pending work is recorded, but its stage is unavailable. ${diagnostic}` };
  if (progress?.configured && progress.closed) return { text: 'Closed' };
  if (stopped) {
    // A known admission hold is more useful than a generic process alert.
    if (attempt.state === 'held' && attempt.reason === 'predecessor_unfinished') return { text: 'Dependency pending', description: `Predecessor work holds this attempt. No retry is confirmed. ${diagnostic}` };
    return { text: 'Needs agent attention', kind: 'attention', description: `The review attempt stopped. This is a process issue, not a PR review verdict. ${progress ? 'No pending recovery is recorded.' : 'Current recovery status is unavailable.'} ${diagnostic}` };
  }
  if (attempt?.state === 'unknown') return { text: 'Status unknown', kind: 'attention', description: diagnostic };
  if (position) return position;
  if (attempt?.state === 'cancelled') return { text: 'Attempt cancelled', kind: 'history', description: diagnostic };
  return undefined;
}
