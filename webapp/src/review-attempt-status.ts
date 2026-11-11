import type { ReviewAttemptSummary } from './types';
import { validActivityEvidence, type ReviewActivityEvidence, type ReviewRoundProgress } from './review-progress';
import type { ReviewChannelPosition } from './review-fronts';

export interface ReviewAttemptStatus {
  text: string;
  description?: string;
  cause?: string;
  kind?: 'attention' | 'history';
  activity?: { startedAt: string; explanation: string; evidence?: ReviewActivityEvidence };
}

// The browser attempt timestamp is the first journal transition (preparation),
// not provider execution. In particular, CLI admission precedes process launch
// and Hosted activity follows trigger delivery, not a provider start receipt.
function activeAttemptStatus(attempt: ReviewAttemptSummary, progress?: ReviewRoundProgress): ReviewAttemptStatus {
  const startedAt = attempt.started_at && Number.isFinite(Date.parse(attempt.started_at)) ? attempt.started_at : '';
  const timing = startedAt ? `Attempt began ${new Date(startedAt).toLocaleString(undefined, { timeZoneName: 'short' })} (${startedAt}).` : 'Attempt start time is unknown.';
  const candidate = progress?.activity_evidence;
  const evidence = attempt.state === 'active' && progress?.channel === attempt.channel && validActivityEvidence(candidate, attempt.channel) && candidate.attempt_id === attempt.source_attempt_id && candidate.head_sha === attempt.head_sha && (!startedAt || Date.parse(candidate.observed_at) >= Date.parse(startedAt)) && (candidate.kind !== 'hosted_processing_observed' || candidate.trigger_id === attempt.trigger_id) ? candidate : undefined;
  const explanation = evidence?.kind === 'cli_process_spawned'
    ? 'CLI process launch was confirmed locally. This does not establish when CodeRabbit’s remote analysis started or whether it is still running.'
    : evidence?.kind === 'hosted_processing_observed'
      ? 'CodeRabbit reported processing this attempt’s matching head and run. It had started by the observation time; this is not an exact provider start time. Processing may have finished since this observation while FireController waits for the result.'
      : attempt.state === 'active'
    ? attempt.channel === 'hosted'
      ? `${attempt.trigger_id ? 'The Hosted review trigger is recorded as sent.' : 'The Hosted attempt is recorded as active; trigger delivery is not shown in this record.'} CodeRabbit may be queued, running, or finished while FireController waits for a result. Actual provider start time is not available.`
      : attempt.channel === 'cli'
        ? 'The CLI attempt is marked active before final checks and process launch. It may still be starting or running, or its result may be awaiting collection. Actual CLI process start time is not available.'
        : 'This attempt is recorded as active. Actual reviewer start time is not available.'
    : attempt.state === 'posting'
      ? 'The review trigger is being posted. Delivery and provider execution are not yet confirmed.'
      : 'This attempt is being prepared. Provider execution is not yet confirmed.';
  const details = `${explanation} The attempt time can include preparation; it is not proof of reviewer start. This is retained status, not a live activity check.`;
  const signal = evidence?.kind === 'cli_process_spawned' ? `CLI process started ${evidence.process_started_at}. ` : evidence ? `CodeRabbit processing observed ${evidence.observed_at}. Provider comment updated ${evidence.comment_updated_at}. ` : '';
  return { text: attempt.state === 'active' ? 'Reviewing' : 'Preparing', description: `${signal}${timing} ${details}`, activity: { startedAt, explanation: details, ...(evidence ? { evidence } : {}) } };
}

// Keep the reason readable without requiring a mouse hover. Raw codes remain
// in the existing Attempts details, including unknown future reason codes.
export function reviewAttemptCause(attempt: ReviewAttemptSummary): string {
  const causes: Record<string, string> = {
    predecessor_unfinished: 'Waiting for earlier PR work',
    admission_changed: 'Review eligibility changed before start',
    source_admission_changed: 'Source eligibility changed before start',
    source_changed: 'The review comparison changed',
    workspace_changed: 'The prepared workspace changed before start',
    execution_binding_changed: 'The execution binding changed before start',
    preparation_missing: 'The prepared review is no longer available',
    hosted_finished_inventory_unproven: 'The finished review could not be verified',
    hosted_finished_incomplete: 'Review output was incomplete; capacity released',
    hosted_allowance_cooldown: 'Hosted allowance stopped this attempt',
    allowance_exhausted: 'Included CLI quota was exhausted',
    allowance_unavailable: 'CLI quota could not be checked',
    process_not_started: 'The review process could not start',
    execution_interrupted: 'The review process was interrupted',
    capture_retention_failed: 'The review result could not be retained',
    source_obligations_pending: 'Carried review work is still pending',
    routed_findings_pending: 'Incoming findings still need handling',
    review_evidence_pending: 'Earlier review results still need handling',
  };
  if (attempt.reason && Object.hasOwn(causes, attempt.reason)) return causes[attempt.reason];
  if (attempt.reason) {
    const words = attempt.reason.replaceAll('_', ' ');
    return `${words[0].toUpperCase()}${words.slice(1)}`;
  }
  return attempt.state === 'uncertain' ? 'The provider outcome is unconfirmed' : 'No reason was recorded';
}

// An attempt is an execution record, not a verdict on the PR. Only retained
// pending work establishes recovery; queue eligibility alone never does.
export function reviewAttemptStatus(attempt?: ReviewAttemptSummary, progress?: ReviewRoundProgress, position?: ReviewChannelPosition, historical = false): ReviewAttemptStatus | undefined {
  const diagnostic = attempt ? `Retained attempt: ${attempt.state}${attempt.reason ? ` (${attempt.reason})` : ''}. See Attempts in review details.` : '';
  const stopped = attempt && ['failed', 'held', 'rate_limited'].includes(attempt.state);
  if (historical) {
    if (!attempt || attempt.state === 'completed') return undefined;
    return { text: 'Past attempt', kind: 'history', cause: stopped || ['uncertain', 'unknown'].includes(attempt.state) ? reviewAttemptCause(attempt) : undefined, description: `${diagnostic} ${attempt.state === 'uncertain' ? 'The provider outcome remains unconfirmed; PR closure does not reconcile it. ' : ''}Historical record, not current activity.` };
  }
  // An unconfirmed provider outcome must be reconciled, even when another
  // request or a closed grant exists. Do not present it as safe to retry.
  if (attempt?.state === 'uncertain') return { text: 'Needs agent attention', kind: 'attention', cause: reviewAttemptCause(attempt), description: `The provider outcome is unconfirmed. Reconcile it before retrying. ${diagnostic}` };
  if (progress?.wait) return undefined; // Render the actual retained wait badge.
  if (attempt && !attempt.finished_at && ['preparing', 'prepared', 'posting', 'active'].includes(attempt.state)) {
    return activeAttemptStatus(attempt, progress);
  }
  if ((progress?.pending_execution ?? 0) > 0) return {
    text: stopped ? 'Retry pending' : 'Pending',
    cause: stopped ? `Previous attempt: ${reviewAttemptCause(attempt)}` : undefined,
    description: stopped ? `A review request is pending after the stopped attempt. This does not establish that a provider review has started. ${diagnostic}` : undefined,
  };
  if ((progress?.pending_adjudication ?? 0) > 0) return { text: 'Needs adjudication' };
  // Older servers expose only a combined pending count. It cannot prove a
  // retry, a running provider, or that the previous failure was resolved.
  if ((progress?.pending ?? 0) > 0) return { text: 'Pending', description: `Pending work is recorded, but its stage is unavailable. ${diagnostic}` };
  if (progress?.configured && progress.closed) return { text: 'Closed' };
  if (stopped) {
    // A known admission hold is more useful than a generic process alert.
    if (attempt.state === 'held' && attempt.reason === 'predecessor_unfinished') return { text: 'Dependency pending', cause: reviewAttemptCause(attempt), description: `Predecessor work holds this attempt. No retry is confirmed. ${diagnostic}` };
    return { text: 'Needs agent attention', kind: 'attention', cause: reviewAttemptCause(attempt), description: `The review attempt stopped. This is a process issue, not a PR review verdict. ${progress ? 'No pending recovery is recorded.' : 'Current recovery status is unavailable.'} ${diagnostic}` };
  }
  if (attempt?.state === 'unknown') return { text: 'Status unknown', kind: 'attention', cause: reviewAttemptCause(attempt), description: diagnostic };
  if (position) return position;
  if (attempt?.state === 'cancelled') return { text: 'Attempt cancelled', kind: 'history', description: diagnostic };
  return undefined;
}
