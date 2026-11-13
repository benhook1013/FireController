import { sessionExpiry } from './browser-session';
import { validOperationWait, type ReviewOperationWait } from './review-operation-wait';

interface ReviewActivityIdentity { attempt_id: string; head_sha: string; observed_at: string }
export interface ReviewCancelledWait { baseline_attempt_id: string; head_sha: string; cancelled_at: string }
export interface ReviewSourceRetry { attempt_id: string; observed_at: string; retry_not_before: string; sequence: number }
export type ReviewActivityEvidence = ReviewActivityIdentity & (
  { kind: 'cli_process_spawned'; process_started_at: string } |
  { kind: 'hosted_processing_observed'; comment_updated_at: string; source_comment_id: number; provider_run_id: string; trigger_id: string }
);

export interface ReviewRoundProgress {
  channel: 'hosted' | 'cli'; configured: boolean; revision: number; epoch: number;
  closed: boolean; minimum: number; maximum: number; maximum_unbounded?: boolean; taper: number;
  used: number; dry: number; tapered: boolean; pending: number;
  pending_execution?: number; pending_adjudication?: number; fixes_pending?: boolean;
  wait?: ReviewOperationWait;
  cancelled_wait?: ReviewCancelledWait;
  activity_evidence?: ReviewActivityEvidence;
  source_retry?: ReviewSourceRetry;
}
export interface ReviewProgressItem { pull_number: number; channels: ReviewRoundProgress[] }
export interface ReviewProgress { project_id: string; evaluated_at: string; items: ReviewProgressItem[] }

export function progressPulls(value: unknown): value is number[] {
  return Array.isArray(value) && value.length > 0 && value.length <= 100 &&
    value.every(number => Number.isSafeInteger(number) && number > 0 && number <= 2147483647) && new Set(value).size === value.length;
}
function object(value: unknown): value is Record<string, unknown> { return !!value && typeof value === 'object' && !Array.isArray(value); }
function fields(value: Record<string, unknown>, keys: string[]) { return Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key)); }
function natural(value: unknown): value is number { return Number.isSafeInteger(value) && (value as number) >= 0; }
function validSourceRetry(value: unknown): value is ReviewSourceRetry {
  return object(value) && fields(value, ['attempt_id', 'observed_at', 'retry_not_before', 'sequence']) &&
    typeof value.attempt_id === 'string' && /^review_attempt_[a-f0-9]{32}$/u.test(value.attempt_id) &&
    !!sessionExpiry(value.observed_at) && !!sessionExpiry(value.retry_not_before) &&
    Date.parse(String(value.retry_not_before)) >= Date.parse(String(value.observed_at)) &&
    natural(value.sequence) && value.sequence >= 1 && value.sequence <= 3;
}
export function validCancelledWait(value: unknown): value is ReviewCancelledWait {
  return object(value) && fields(value, ['baseline_attempt_id', 'head_sha', 'cancelled_at']) && typeof value.baseline_attempt_id === 'string' && /^review_attempt_[a-f0-9]{32}$/u.test(value.baseline_attempt_id) && typeof value.head_sha === 'string' && /^[a-f0-9]{40}$/u.test(value.head_sha) && !!sessionExpiry(value.cancelled_at);
}
export function validActivityEvidence(value: unknown, channel: string): value is ReviewActivityEvidence {
  if (!object(value) || typeof value.attempt_id !== 'string' || !/^review_attempt_[a-f0-9]{32}$/u.test(value.attempt_id) || typeof value.head_sha !== 'string' || !/^[a-f0-9]{40}$/u.test(value.head_sha) || !sessionExpiry(value.observed_at)) return false;
  const identity = ['attempt_id', 'head_sha', 'kind', 'observed_at'];
  if (channel === 'cli') return fields(value, [...identity, 'process_started_at']) && value.kind === 'cli_process_spawned' && !!sessionExpiry(value.process_started_at) && Date.parse(String(value.process_started_at)) <= Date.parse(String(value.observed_at));
  return channel === 'hosted' && fields(value, [...identity, 'comment_updated_at', 'source_comment_id', 'provider_run_id', 'trigger_id']) && value.kind === 'hosted_processing_observed' && !!sessionExpiry(value.comment_updated_at) && Date.parse(String(value.comment_updated_at)) <= Date.parse(String(value.observed_at)) && Number.isSafeInteger(value.source_comment_id) && Number(value.source_comment_id) > 0 && typeof value.provider_run_id === 'string' && /^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/u.test(value.provider_run_id) && typeof value.trigger_id === 'string' && /^[1-9][0-9]{0,18}$/u.test(value.trigger_id);
}
export function validRoundProgress(value: unknown, channel: string): value is ReviewRoundProgress {
  if (!object(value) || !fields(value, ['channel','configured','revision','epoch','closed','minimum','maximum','taper','used','dry','tapered','pending', ...(Object.hasOwn(value, 'maximum_unbounded') ? ['maximum_unbounded'] : []), ...(Object.hasOwn(value, 'pending_execution') || Object.hasOwn(value, 'pending_adjudication') ? ['pending_execution', 'pending_adjudication'] : []), ...(Object.hasOwn(value, 'fixes_pending') ? ['fixes_pending'] : []), ...(Object.hasOwn(value, 'wait') ? ['wait'] : []), ...(Object.hasOwn(value, 'cancelled_wait') ? ['cancelled_wait'] : []), ...(Object.hasOwn(value, 'activity_evidence') ? ['activity_evidence'] : []), ...(Object.hasOwn(value, 'source_retry') ? ['source_retry'] : [])]) || (Object.hasOwn(value, 'maximum_unbounded') && typeof value.maximum_unbounded !== 'boolean') || value.channel !== channel || typeof value.configured !== 'boolean' || typeof value.closed !== 'boolean' || typeof value.tapered !== 'boolean') return false;
  if (Object.hasOwn(value, 'fixes_pending') && (typeof value.fixes_pending !== 'boolean' || !Object.hasOwn(value, 'pending_execution'))) return false;
  if (Object.hasOwn(value, 'wait') && !validOperationWait(value.wait, channel)) return false;
  if (Object.hasOwn(value, 'cancelled_wait') && (!Object.hasOwn(value, 'pending_execution') || !validCancelledWait(value.cancelled_wait))) return false;
  if (Object.hasOwn(value, 'activity_evidence') && !validActivityEvidence(value.activity_evidence, channel)) return false;
  if (Object.hasOwn(value, 'source_retry') && !validSourceRetry(value.source_retry)) return false;
  const numbers = ['revision','epoch','minimum','maximum','taper','used','dry','pending'] as const;
  if (!numbers.every(key => natural(value[key]))) return false;
  if (Object.hasOwn(value, 'pending_execution') && (!natural(value.pending_execution) || !natural(value.pending_adjudication) || value.pending_execution > Number(value.pending) || value.pending_adjudication > Number(value.pending) || value.pending_execution + value.pending_adjudication !== value.pending)) return false;
  const c = value as unknown as ReviewRoundProgress;
  if (!c.configured) return !c.maximum_unbounded && !c.closed && !c.tapered && numbers.every(key => c[key] === 0);
  // Adopted grants may start with no allowance or with a prior dry streak.
  return c.revision > 0 && c.epoch > 0 && c.epoch <= c.revision && (c.maximum_unbounded ? c.maximum === 0 : c.minimum <= c.maximum && c.used <= c.maximum) && c.taper >= 1 && c.taper <= 1000 && c.dry - c.used <= 1000 && c.pending <= 32 &&
    (!c.tapered || c.used >= c.minimum);
}
export function validReviewProgress(value: unknown, project: string, pulls: number[]): value is ReviewProgress {
  if (!object(value) || !fields(value, ['project_id','evaluated_at','items']) || value.project_id !== project || !sessionExpiry(value.evaluated_at) || !Array.isArray(value.items) || value.items.length !== pulls.length) return false;
  return value.items.every((item, index) => object(item) && fields(item, ['pull_number','channels']) && item.pull_number === pulls[index] && Array.isArray(item.channels) && item.channels.length === 2 && validRoundProgress(item.channels[0], 'hosted') && validRoundProgress(item.channels[1], 'cli'));
}
export function roundProgressState(c: ReviewRoundProgress): string {
  return roundProgressStopReason(c) ? 'No reviews remaining' : 'More rounds allowed';
}

export function roundProgressStopReason(c: ReviewRoundProgress): string | undefined {
  if (c.closed) return 'Manually closed';
  if (c.tapered) return 'Taper met';
  if (!c.maximum_unbounded && c.used >= c.maximum) return 'Maximum reached';
  return undefined;
}

export function roundProgressAllowance(c: ReviewRoundProgress): string {
  if (c.maximum_unbounded) return 'No maximum';
  const remaining = c.maximum - c.used;
  return remaining > 0 ? `Up to ${remaining} more ${remaining === 1 ? 'review' : 'reviews'}` : 'No reviews remaining';
}
