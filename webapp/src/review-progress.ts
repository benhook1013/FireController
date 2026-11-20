import { sessionExpiry } from './browser-session';
import { validOperationWait, type ReviewOperationWait } from './review-operation-wait';

export interface ReviewRoundProgress {
  channel: 'hosted' | 'cli';
  configured: boolean;
  revision: number;
  epoch: number;
  closed: boolean;
  minimum: number;
  maximum: number;
  maximum_unbounded?: boolean;
  taper: number;
  used: number;
  dry: number;
  tapered: boolean;
  pending: number;
  pending_execution?: number;
  pending_adjudication?: number;
  wait?: ReviewOperationWait;
}

export interface ReviewProgressItem { pull_number: number; channels: ReviewRoundProgress[] }
export interface ReviewProgress { project_id: string; evaluated_at: string; items: ReviewProgressItem[] }

export function progressPulls(value: unknown): value is number[] {
  return Array.isArray(value) && value.length > 0 && value.length <= 100 &&
    value.every(number => Number.isSafeInteger(number) && number > 0 && number <= 2147483647) && new Set(value).size === value.length;
}

function object(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value);
}

function fields(value: Record<string, unknown>, required: string[], optional: string[] = []): boolean {
  return required.every(key => Object.hasOwn(value, key)) &&
    Object.keys(value).every(key => required.includes(key) || optional.includes(key));
}

function natural(value: unknown): value is number { return Number.isSafeInteger(value) && Number(value) >= 0; }

export function validRoundProgress(value: unknown, channel: string): value is ReviewRoundProgress {
  if (!object(value) || !fields(value,
    ['channel', 'configured', 'revision', 'epoch', 'closed', 'minimum', 'maximum', 'taper', 'used', 'dry', 'tapered', 'pending'],
    ['maximum_unbounded', 'pending_execution', 'pending_adjudication', 'wait']) ||
    (Object.hasOwn(value, 'maximum_unbounded') && typeof value.maximum_unbounded !== 'boolean') ||
    (Object.hasOwn(value, 'pending_execution') !== Object.hasOwn(value, 'pending_adjudication')) ||
    value.channel !== channel || typeof value.configured !== 'boolean' || typeof value.closed !== 'boolean' || typeof value.tapered !== 'boolean') return false;
  if (Object.hasOwn(value, 'wait') && !validOperationWait(value.wait, channel)) return false;
  const numeric = ['revision', 'epoch', 'minimum', 'maximum', 'taper', 'used', 'dry', 'pending'] as const;
  if (!numeric.every(key => natural(value[key]))) return false;
  if (Object.hasOwn(value, 'pending_execution') &&
    (!natural(value.pending_execution) || !natural(value.pending_adjudication) ||
      Number(value.pending_execution) > Number(value.pending) || Number(value.pending_adjudication) > Number(value.pending) ||
      Number(value.pending_execution) + Number(value.pending_adjudication) !== Number(value.pending))) return false;
  const progress = value as unknown as ReviewRoundProgress;
  if (!progress.configured) {
    return !progress.maximum_unbounded && !progress.closed && !progress.tapered && numeric.every(key => progress[key] === 0) &&
      (!Object.hasOwn(value, 'pending_execution') || progress.pending_execution === 0 && progress.pending_adjudication === 0);
  }
  return progress.revision > 0 && progress.epoch > 0 && progress.epoch <= progress.revision &&
    (progress.maximum_unbounded ? progress.maximum === 0 : progress.minimum <= progress.maximum && progress.used <= progress.maximum) &&
    progress.taper >= 1 && progress.taper <= 1000 && progress.dry - progress.used <= 1000 && progress.pending <= 32 &&
    (!progress.tapered || progress.used >= progress.minimum);
}

export function validReviewProgress(value: unknown, project: string, pulls: number[]): value is ReviewProgress {
  if (!object(value) || !fields(value, ['project_id', 'evaluated_at', 'items']) || value.project_id !== project ||
    !sessionExpiry(value.evaluated_at) || !Array.isArray(value.items) || value.items.length !== pulls.length) return false;
  return value.items.every((item, index) => object(item) && fields(item, ['pull_number', 'channels']) &&
    item.pull_number === pulls[index] && Array.isArray(item.channels) && item.channels.length === 2 &&
    validRoundProgress(item.channels[0], 'hosted') && validRoundProgress(item.channels[1], 'cli'));
}

export function roundProgressState(value: ReviewRoundProgress): string {
  return roundProgressStopReason(value) ? 'No reviews remaining' : 'More rounds allowed';
}

export function roundProgressStopReason(value: ReviewRoundProgress): string | undefined {
  if (value.closed) return 'Manually closed';
  if (value.tapered) return 'Taper met';
  if (!value.maximum_unbounded && value.used >= value.maximum) return 'Maximum reached';
  return undefined;
}

export function roundProgressAllowance(value: ReviewRoundProgress): string {
  if (value.maximum_unbounded) return 'No maximum';
  const remaining = value.maximum - value.used;
  return remaining > 0 ? `Up to ${remaining} more ${remaining === 1 ? 'review' : 'reviews'}` : 'No reviews remaining';
}
