import { sessionExpiry } from './browser-session';

export interface ReviewRoundProgress {
  channel: 'hosted' | 'cli'; configured: boolean; revision: number; epoch: number;
  closed: boolean; minimum: number; maximum: number; maximum_unbounded?: boolean; taper: number;
  used: number; dry: number; tapered: boolean; pending: number;
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
export function validRoundProgress(value: unknown, channel: string): value is ReviewRoundProgress {
  if (!object(value) || !fields(value, ['channel','configured','revision','epoch','closed','minimum','maximum','taper','used','dry','tapered','pending', ...(Object.hasOwn(value, 'maximum_unbounded') ? ['maximum_unbounded'] : [])]) || (Object.hasOwn(value, 'maximum_unbounded') && typeof value.maximum_unbounded !== 'boolean') || value.channel !== channel || typeof value.configured !== 'boolean' || typeof value.closed !== 'boolean' || typeof value.tapered !== 'boolean') return false;
  const numbers = ['revision','epoch','minimum','maximum','taper','used','dry','pending'] as const;
  if (!numbers.every(key => natural(value[key]))) return false;
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
