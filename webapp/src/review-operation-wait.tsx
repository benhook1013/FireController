import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { sessionExpiry } from './browser-session';

export interface CLIAllowance {
  kind: 'cli_exhausted';
  observed_at: string;
  remaining: number;
  limit: number;
  full_capacity_in_ms: number;
  rolling_window_ms: number;
}

export interface ReviewOperationWait {
  queue_ahead_pull_number?: number;
  baseline_attempt_id?: string;
  retry_not_before?: string;
  paused: boolean;
  cli_allowance?: CLIAllowance;
}

export function validOperationWait(value: unknown, channel: string): value is ReviewOperationWait {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const wait = value as Record<string, unknown>;
  if (Object.keys(wait).some(key => !['queue_ahead_pull_number', 'baseline_attempt_id', 'retry_not_before', 'paused', 'cli_allowance'].includes(key)) || typeof wait.paused !== 'boolean') return false;
  if (wait.queue_ahead_pull_number !== undefined &&
    (channel !== 'hosted' || wait.paused || !Number.isSafeInteger(wait.queue_ahead_pull_number) || Number(wait.queue_ahead_pull_number) < 1 || Number(wait.queue_ahead_pull_number) > 2147483647)) return false;
  if (wait.cli_allowance !== undefined && (channel !== 'cli' || wait.baseline_attempt_id === undefined || !validCLIAllowance(wait.cli_allowance))) return false;
  if (wait.baseline_attempt_id === undefined) return wait.retry_not_before === undefined;
  if (!['cli', 'hosted'].includes(channel) || typeof wait.baseline_attempt_id !== 'string' ||
    !/^review_attempt_[a-f0-9]{32}$/u.test(wait.baseline_attempt_id) || typeof wait.retry_not_before !== 'string') return false;
  return validTimestamp(wait.retry_not_before);
}

function validCLIAllowance(value: unknown): value is CLIAllowance {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const allowance = value as Record<string, unknown>;
  const fields = ['kind', 'observed_at', 'remaining', 'limit', 'full_capacity_in_ms', 'rolling_window_ms'];
  return Object.keys(allowance).length === fields.length && fields.every(key => Object.hasOwn(allowance, key)) &&
    allowance.kind === 'cli_exhausted' && sessionExpiry(allowance.observed_at) && typeof allowance.observed_at === 'string' && allowance.observed_at.endsWith('Z') &&
    allowance.remaining === 0 && ['limit', 'full_capacity_in_ms', 'rolling_window_ms'].every(key => Number.isSafeInteger(allowance[key])) &&
    Number(allowance.limit) >= 1 && Number(allowance.limit) <= 10000 && Number(allowance.full_capacity_in_ms) >= 0 && Number(allowance.full_capacity_in_ms) <= 86400000 &&
    Number(allowance.rolling_window_ms) >= 1 && Number(allowance.rolling_window_ms) <= 86400000;
}

function validTimestamp(value: string): boolean {
  return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/u.test(value) && Number.isFinite(Date.parse(value));
}

function relativeAge(value: string, now: number): string {
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) return 'Date unknown';
  const elapsed = now - timestamp;
  if (elapsed <= -60000) return 'Future date';
  const minutes = Math.floor(Math.max(0, elapsed) / 60000);
  if (minutes < 1) return 'just now';
  if (minutes < 60) return `${minutes}m ago`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)}h ${minutes % 60}m ago`;
  return `${Math.floor(minutes / 1440)}d ${Math.floor(minutes % 1440 / 60)}h ago`;
}

export function operationWaitLabel(wait: ReviewOperationWait, now = Date.now(), channel = 'hosted'): string {
  if (wait.paused) return 'Paused';
  if (channel === 'hosted' && wait.queue_ahead_pull_number !== undefined) return `Behind #${wait.queue_ahead_pull_number}`;
  if (!wait.retry_not_before) return 'Waiting';
  if (channel === 'cli') return 'Quota wait';
  const minutes = Math.ceil((Date.parse(wait.retry_not_before) - now) / 60000);
  const remaining = minutes >= 60 ? `${Math.floor(minutes / 60)}h ${minutes % 60}m` : `${minutes}m`;
  return minutes > 0 ? `Cooldown · ${remaining}` : 'Waiting';
}

export function reviewWaitExpandedForKey(key: string, expanded: boolean): boolean | undefined {
  if (key === 'Escape') return false;
  if (key === 'Enter' || key === ' ') return !expanded;
  return undefined;
}

export function ReviewOperationWaitBadge({ wait, channel = 'hosted' }: { wait: ReviewOperationWait; channel?: string }) {
  const behind = channel === 'hosted' && wait.queue_ahead_pull_number !== undefined && !wait.paused;
  const [now, setNow] = useState(Date.now);
  const [expanded, setExpanded] = useState(false);
  const detailsID = useId();
  const trigger = useRef<HTMLSpanElement>(null);
  const panel = useRef<HTMLSpanElement>(null);
  const [placement, setPlacement] = useState({ left: 8, top: 8, maxHeight: 440 });
  useLayoutEffect(() => {
    if (!expanded) return;
    const place = () => {
      const rect = trigger.current?.getBoundingClientRect();
      const content = panel.current;
      if (!rect || !content) return;
      const width = Math.min(360, window.innerWidth - 16);
      const below = Math.max(0, window.innerHeight - rect.bottom - 8);
      const above = Math.max(0, rect.top - 8);
      const preferred = Math.min(440, window.innerHeight * 0.7, content.scrollHeight + 2);
      const openAbove = below < preferred + 8 && above > below;
      const maxHeight = Math.max(0, Math.min(preferred, (openAbove ? above : below) - 8));
      setPlacement({
        left: Math.max(8, Math.min(rect.left, window.innerWidth - width - 8)),
        top: openAbove ? rect.top - maxHeight - 8 : rect.bottom,
        maxHeight,
      });
    };
    place();
    window.addEventListener('resize', place);
    window.addEventListener('scroll', place, true);
    return () => { window.removeEventListener('resize', place); window.removeEventListener('scroll', place, true); };
  }, [expanded]);
  useEffect(() => {
    if (behind || !wait.retry_not_before) return;
    const timer = setInterval(() => setNow(Date.now()), 10000);
    return () => clearInterval(timer);
  }, [behind, wait.retry_not_before]);

  if (behind) {
    return <span className="badge review-channel-position" title={`Queued behind PR #${wait.queue_ahead_pull_number}. Its accepted Hosted request has priority. This review has not started.`}>{operationWaitLabel(wait, now, channel)}</span>;
  }

  const description = wait.paused
    ? 'This review request is paused before dispatch. Its retry deadline is retained; resume is explicit.'
    : channel === 'cli' && wait.retry_not_before
      ? 'Included CLI quota is exhausted. This request remains queued; the next automatic quota check follows the retained recheck time below. This is a server recheck time, not a provider reset or promised start. No review is running.'
      : wait.retry_not_before
        ? `This same request automatically rechecks at or after ${new Date(wait.retry_not_before).toLocaleString()}. This is the earliest retry, not a promised review start. No review is running.`
        : 'This request is waiting before dispatch. No review is running.';
  const allowance = channel === 'cli' ? wait.cli_allowance : undefined;
  const estimate = allowance ? Date.parse(allowance.observed_at) + allowance.full_capacity_in_ms : undefined;
  if (!wait.retry_not_before) return <span className="badge review-channel-position" title={description}>{operationWaitLabel(wait, now, channel)}</span>;

  const retry = Date.parse(wait.retry_not_before);
  const relative = (stamp: number) => {
    if (stamp <= now) return relativeAge(new Date(stamp).toISOString(), now);
    const minutes = Math.ceil((stamp - now) / 60000);
    return `in ${minutes >= 60 ? `${Math.floor(minutes / 60)}h ${minutes % 60}m` : `${minutes}m`}`;
  };
  return <span className="review-quota-wait" onPointerEnter={event => { if (event.pointerType === 'mouse') setExpanded(true); }} onPointerLeave={event => { if (event.pointerType === 'mouse' && !event.currentTarget.contains(document.activeElement)) setExpanded(false); }} onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) setExpanded(false); }} onKeyDown={event => { if (event.key === 'Escape') { trigger.current?.focus(); setExpanded(false); } }}>
    <span ref={trigger} role="button" aria-expanded={expanded} aria-controls={detailsID} tabIndex={0} className="badge review-channel-position review-quota-label" aria-describedby={detailsID} onFocus={event => { if (event.currentTarget.matches(':focus-visible')) setExpanded(true); }} onPointerUp={event => { if (event.pointerType !== 'mouse') { event.stopPropagation(); setExpanded(value => !value); } }} onClick={event => event.stopPropagation()} onKeyDown={event => { const next = reviewWaitExpandedForKey(event.key, expanded); if (next !== undefined && event.key !== 'Escape') { event.preventDefault(); event.stopPropagation(); setExpanded(next); } }}>{operationWaitLabel(wait, now, channel)}</span>
    <span id={detailsID} className="review-quota-panel" style={{ left: placement.left, top: placement.top }} hidden={!expanded}>
      <span ref={panel} tabIndex={0} role="region" aria-label="Review wait details" className="review-quota-details" style={{ maxHeight: placement.maxHeight }}>
        <span>{description}</span>
        {estimate !== undefined && <span>Provider full-capacity estimate: <time dateTime={new Date(estimate).toISOString()} title={new Date(estimate).toLocaleString()}>{relative(estimate)}</time>{estimate <= now ? ' (past estimate)' : ''}. <small>{new Date(estimate).toLocaleString()}</small></span>}
        <span>{wait.paused ? 'Retained recheck' : channel === 'cli' ? 'Next quota recheck' : 'Earliest retry'}: <time dateTime={wait.retry_not_before} title={new Date(retry).toLocaleString()}>{relative(retry)}</time>{retry <= now ? ' (awaiting recheck)' : ''}. <small>{new Date(retry).toLocaleString()}</small></span>
        {estimate !== undefined && <span>The provider estimate does not establish the next available slot, refill pattern or review start. {estimate <= now ? 'The retained estimate is in the past; current quota has not been confirmed.' : 'Actual availability must be rechecked.'}</span>}
      </span>
    </span>
  </span>;
}
