import type { ReviewAttemptSummary } from './types';
import { validActivityEvidence, validCancelledWait, type ReviewActivityEvidence, type ReviewRoundProgress } from './review-progress';
import { reviewFixesPending, type ReviewChannelPosition } from './review-fronts';

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
  const candidate = progress?.activity_evidence;
  const evidence = attempt.state === 'active' && progress?.channel === attempt.channel && validActivityEvidence(candidate, attempt.channel) && candidate.attempt_id === attempt.source_attempt_id && candidate.head_sha === attempt.head_sha && (!startedAt || Date.parse(candidate.observed_at) >= Date.parse(startedAt)) && (candidate.kind !== 'hosted_processing_observed' || candidate.trigger_id === attempt.trigger_id) ? candidate : undefined;
  const explanation = evidence?.kind === 'cli_process_spawned'
    ? 'Waiting for the CLI result.'
    : evidence?.kind === 'hosted_processing_observed'
      ? 'Waiting for the Hosted result.'
      : attempt.state === 'active'
        ? attempt.channel === 'hosted' && attempt.trigger_id
          ? 'Review requested. Waiting for CodeRabbit to report progress.'
          : 'Waiting for confirmation that the review started.'
        : attempt.state === 'posting'
          ? 'Sending the review request to CodeRabbit.'
          : 'Preparing the review.';
  return { text: attempt.state === 'active' ? 'Reviewing' : 'Preparing', activity: { startedAt, explanation, ...(evidence ? { evidence } : {}) } };
}

const githubRateLimit = 'GitHub rate limit. The review did not start.';
const sourceRateLimitWait = 'source_rate_limit_wait';
function isGitHubPreparationHold(reason?: string): boolean {
  return reason === 'preparation_source_resolution_provider_rate_limited' || reason === 'preparation_source_refresh_provider_rate_limited';
}

// Use human-readable reasons in tooltips and Attempts.
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
    cli_partial_settled: 'The CLI review ended without full coverage',
    source_rate_limit_wait: 'GitHub rate limit delayed preparation',
    cli_partial_comparison_already_reviewed: 'Automatic CLI is waiting for the comparison to change',
    capture_retained: 'Review output is available in history',
    capture_retention_failed: 'The review result could not be retained',
    source_obligations_pending: 'Carried review work is still pending',
    routed_findings_pending: 'Incoming findings still need handling',
    review_evidence_pending: 'Earlier review results still need handling',
  };
  if (isGitHubPreparationHold(attempt.reason)) return githubRateLimit;
  if (attempt.reason && Object.hasOwn(causes, attempt.reason)) return causes[attempt.reason];
  if (attempt.reason) {
    const words = attempt.reason.replaceAll('_', ' ');
    return `${words[0].toUpperCase()}${words.slice(1)}`.replace(/\b(?:ci|cli)\b/giu, acronym => acronym.toUpperCase());
  }
  return attempt.state === 'uncertain' ? 'The provider outcome is unconfirmed' : 'No reason was recorded';
}

// An attempt is an execution record, not a verdict on the PR. Only retained
// pending work establishes recovery; queue eligibility alone never does.
export function reviewAttemptStatus(attempt?: ReviewAttemptSummary, progress?: ReviewRoundProgress, position?: ReviewChannelPosition, historical = false): ReviewAttemptStatus | undefined {
  const diagnostic = attempt ? `Retained attempt: ${attempt.state}${attempt.reason ? ` (${attempt.reason})` : ''}. See Attempts in review details.` : '';
  const stopped = attempt && ['failed', 'held', 'rate_limited'].includes(attempt.state);
  const settled = attempt?.state === 'failed' && attempt.reason === 'cli_partial_settled';
  if (historical) {
    if (!attempt || attempt.state === 'completed' || settled) return undefined;
    return { text: 'Past attempt', kind: 'history', cause: stopped || ['uncertain', 'unknown'].includes(attempt.state) ? reviewAttemptCause(attempt) : undefined, description: `${diagnostic} ${attempt.state === 'uncertain' ? 'The provider outcome remains unconfirmed; PR closure does not reconcile it. ' : ''}Historical record, not current activity.` };
  }
  // An unconfirmed provider outcome must be reconciled, even when another
  // request or a closed grant exists. Do not present it as safe to retry.
  if (attempt?.state === 'uncertain') return { text: 'Needs agent attention', kind: 'attention', cause: reviewAttemptCause(attempt), description: `The provider outcome is unconfirmed. Reconcile it before retrying. ${diagnostic}` };
  if (progress?.retained_adjudication_pending && !(attempt && !attempt.finished_at && ['preparing', 'prepared', 'posting', 'active'].includes(attempt.state))) return { text: 'Needs adjudication', description: 'Retained partial findings need decisions. No completed review round is claimed.' };
  if (progress?.wait && (!progress.source_retry || progress.wait.paused)) return undefined; // Render the actual retained wait badge.
  if (attempt && !attempt.finished_at && ['preparing', 'prepared', 'posting', 'active'].includes(attempt.state)) {
    return activeAttemptStatus(attempt, progress);
  }
  if (progress?.source_retry && !progress.wait?.paused) return { text: 'Queued' };
  const githubHold = stopped && isGitHubPreparationHold(attempt.reason);
  const priorSourceWait = stopped && attempt.reason === sourceRateLimitWait;
  if ((githubHold || priorSourceWait) && (progress?.pending_execution ?? 0) > 0) return { text: 'Queued' };
  if ((progress?.pending_execution ?? 0) > 0) return {
    text: stopped && !settled ? 'Retry pending' : 'Pending',
    cause: undefined,
    description: stopped && !settled ? `A review request is pending after the stopped attempt. This does not establish that a provider review has started. ${diagnostic}` : undefined,
  };
  if ((progress?.pending_adjudication ?? 0) > 0) return { text: 'Needs adjudication' };
  if (progress?.fixes_pending) return reviewFixesPending;
  // Older servers expose only a combined pending count. It cannot prove a
  // retry, a running provider, or that the previous failure was resolved.
  if ((progress?.pending ?? 0) > 0) return { text: 'Pending', description: `Pending work is recorded, but its stage is unavailable.${settled ? '' : ` ${diagnostic}`}` };
  if (progress?.configured && progress.closed) return { text: 'Closed' };
  const cancelled = progress?.cancelled_wait;
  if (attempt?.state === 'held' && progress?.channel === attempt.channel && progress.pending_execution === 0 && progress.pending_adjudication === 0 && validCancelledWait(cancelled) && cancelled.baseline_attempt_id === attempt.source_attempt_id && cancelled.head_sha === attempt.head_sha && attempt.reason === (attempt.channel === 'hosted' ? 'hosted_allowance_cooldown' : 'allowance_exhausted') && !attempt.source_run_id && !attempt.trigger_id && !attempt.provider_review_id && attempt.finished_at && Number.isFinite(Date.parse(attempt.finished_at)) && Date.parse(attempt.finished_at) <= Date.parse(cancelled.cancelled_at)) {
    return undefined; // The request is gone; its unused baseline stays in history.
  }
  if (stopped && !settled && !priorSourceWait) {
    if (githubHold) return { text: 'Needs agent attention', kind: 'attention', cause: githubRateLimit, description: 'See Attempts in review history.' };
    // A known admission hold is more useful than a generic process alert.
    if (attempt.state === 'held' && attempt.reason === 'predecessor_unfinished') return { text: 'Dependency pending', cause: reviewAttemptCause(attempt), description: `Predecessor work holds this attempt. No retry is confirmed. ${diagnostic}` };
    return { text: 'Needs agent attention', kind: 'attention', cause: reviewAttemptCause(attempt), description: `The review attempt stopped. This is a process issue, not a PR review verdict. ${progress ? 'No pending recovery is recorded.' : 'Current recovery status is unavailable.'} ${diagnostic}` };
  }
  if (attempt?.state === 'unknown') return { text: 'Status unknown', kind: 'attention', cause: reviewAttemptCause(attempt), description: diagnostic };
  if (position) return position;
  if (attempt?.state === 'cancelled') return { text: 'Attempt cancelled', kind: 'history', description: diagnostic };
  return undefined;
}
