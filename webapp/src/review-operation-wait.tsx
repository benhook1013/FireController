import { useEffect, useState } from 'react';

export interface ReviewOperationWait {
  baseline_attempt_id?: string;
  retry_not_before?: string;
  paused: boolean;
}

export function validOperationWait(value: unknown, channel: string): value is ReviewOperationWait {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const v = value as Record<string, unknown>;
  if (Object.keys(v).some(key => !['baseline_attempt_id', 'retry_not_before', 'paused'].includes(key)) || typeof v.paused !== 'boolean') return false;
  if (v.baseline_attempt_id === undefined) return v.retry_not_before === undefined;
  if (!['cli', 'hosted'].includes(channel) || typeof v.baseline_attempt_id !== 'string' || !/^review_attempt_[a-f0-9]{32}$/u.test(v.baseline_attempt_id) || typeof v.retry_not_before !== 'string') return false;
  return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/u.test(v.retry_not_before) && Number.isFinite(Date.parse(v.retry_not_before));
}

export function operationWaitLabel(wait: ReviewOperationWait, now = Date.now(), channel = 'hosted'): string {
  if (wait.paused) return 'Paused';
  if (!wait.retry_not_before) return 'Waiting';
  if (channel === 'cli') return 'Quota wait';
  const minutes = Math.ceil((Date.parse(wait.retry_not_before) - now) / 60000);
  const remaining = minutes >= 60 ? `${Math.floor(minutes / 60)}h ${minutes % 60}m` : `${minutes}m`;
  return minutes > 0 ? `Cooldown · ${remaining}` : 'Waiting';
}

export function ReviewOperationWaitBadge({ wait, channel = 'hosted' }: { wait: ReviewOperationWait; channel?: string }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 10000); return () => clearInterval(timer); }, []);
  const description = wait.paused
    ? 'This review request is paused before dispatch. Its retry deadline is retained; resume is explicit.'
    : channel === 'cli' && wait.retry_not_before
      ? `Included CLI quota is exhausted. This request remains queued; the next automatic quota check is at or after ${new Date(wait.retry_not_before).toLocaleString()}. This is a server recheck time, not a provider reset or promised start. No review is running.`
      : wait.retry_not_before
      ? `This same request automatically rechecks at or after ${new Date(wait.retry_not_before).toLocaleString()}. This is the earliest retry, not a promised review start. No review is running.`
      : 'This request is waiting before dispatch. No review is running.';
  return <span className="badge review-channel-position" title={description}>{operationWaitLabel(wait, now, channel)}</span>;
}
