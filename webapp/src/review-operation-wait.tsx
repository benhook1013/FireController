import { useEffect, useId, useRef, useState } from 'react';
import { sessionExpiry } from './browser-session';
import { relativeAge } from './relative-time';

export interface CLIAllowance { kind: 'cli_exhausted'; observed_at: string; remaining: number; limit: number; full_capacity_in_ms: number; rolling_window_ms: number }

export interface ReviewOperationWait {
  baseline_attempt_id?: string;
  retry_not_before?: string;
  paused: boolean;
  cli_allowance?: CLIAllowance;
}

export function validOperationWait(value: unknown, channel: string): value is ReviewOperationWait {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const v = value as Record<string, unknown>;
  if (Object.keys(v).some(key => !['baseline_attempt_id', 'retry_not_before', 'paused', 'cli_allowance'].includes(key)) || typeof v.paused !== 'boolean') return false;
  if (v.cli_allowance !== undefined && (channel !== 'cli' || v.baseline_attempt_id === undefined || !validCLIAllowance(v.cli_allowance))) return false;
  if (v.baseline_attempt_id === undefined) return v.retry_not_before === undefined;
  if (!['cli', 'hosted'].includes(channel) || typeof v.baseline_attempt_id !== 'string' || !/^review_attempt_[a-f0-9]{32}$/u.test(v.baseline_attempt_id) || typeof v.retry_not_before !== 'string') return false;
  return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/u.test(v.retry_not_before) && Number.isFinite(Date.parse(v.retry_not_before));
}

function validCLIAllowance(value: unknown): value is CLIAllowance {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const v=value as Record<string,unknown>;
  const fields=['kind','observed_at','remaining','limit','full_capacity_in_ms','rolling_window_ms'];
  return Object.keys(v).length === fields.length && fields.every(key=>Object.hasOwn(v,key)) && v.kind === 'cli_exhausted' && sessionExpiry(v.observed_at) && typeof v.observed_at === 'string' && v.observed_at.endsWith('Z') && v.remaining === 0 && ['limit','full_capacity_in_ms','rolling_window_ms'].every(key=>Number.isSafeInteger(v[key])) && Number(v.limit)>=1 && Number(v.limit)<=10000 && Number(v.full_capacity_in_ms)>=0 && Number(v.full_capacity_in_ms)<=86400000 && Number(v.rolling_window_ms)>=1 && Number(v.rolling_window_ms)<=86400000;
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
  const [expanded, setExpanded] = useState(false);
  const detailsID = useId();
  const button = useRef<HTMLButtonElement>(null);
  const [placement, setPlacement] = useState({ left: 8, top: 8, width: 360 });
  useEffect(() => {
    if (!expanded) return;
    const place = () => {
      const rect = button.current?.getBoundingClientRect();
      if (!rect) return;
      const width = Math.min(360, window.innerWidth - 16);
      setPlacement({ left: Math.max(8, Math.min(rect.left, window.innerWidth - width - 8)), top: Math.min(rect.bottom + 4, window.innerHeight - 80), width });
    };
    place(); window.addEventListener('resize', place); window.addEventListener('scroll', place, true);
    return () => { window.removeEventListener('resize', place); window.removeEventListener('scroll', place, true); };
  }, [expanded]);
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 10000); return () => clearInterval(timer); }, []);
  const description = wait.paused
    ? 'This review request is paused before dispatch. Its retry deadline is retained; resume is explicit.'
    : channel === 'cli' && wait.retry_not_before
      ? `Included CLI quota is exhausted. This request remains queued; the next automatic quota check follows the retained recheck time below. This is a server recheck time, not a provider reset or promised start. No review is running.`
      : wait.retry_not_before
      ? `This same request automatically rechecks at or after ${new Date(wait.retry_not_before).toLocaleString()}. This is the earliest retry, not a promised review start. No review is running.`
      : 'This request is waiting before dispatch. No review is running.';
  const allowance=channel === 'cli' ? wait.cli_allowance : undefined;
  const estimate=allowance ? Date.parse(allowance.observed_at)+allowance.full_capacity_in_ms : undefined;
  if (channel !== 'cli' || !wait.retry_not_before) return <span className="badge review-channel-position" title={description}>{operationWaitLabel(wait, now, channel)}</span>;
  const retry = Date.parse(wait.retry_not_before);
  const relative = (stamp: number) => {
    if (stamp <= now) return relativeAge(new Date(stamp).toISOString(), now);
    const minutes = Math.ceil((stamp - now) / 60000);
    return `in ${minutes >= 60 ? `${Math.floor(minutes / 60)}h ${minutes % 60}m` : `${minutes}m`}`;
  };
  return <span className="review-quota-wait" onPointerEnter={event => { if (event.pointerType === 'mouse') setExpanded(true); }} onPointerLeave={event => { if (!event.currentTarget.contains(document.activeElement)) setExpanded(false); }} onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) setExpanded(false); }} onKeyDown={event => { if (event.key === 'Escape') setExpanded(false); }}>
    <button ref={button} type="button" onFocus={event => { if (event.currentTarget.matches(':focus-visible')) setExpanded(true); }} className="badge review-channel-position" aria-expanded={expanded} aria-controls={detailsID} onClick={() => setExpanded(value => !value)}>{operationWaitLabel(wait, now, channel)}</button>
    <span id={detailsID} className="review-quota-details" style={placement} hidden={!expanded} role="note">
      <span>{description}</span>
      {estimate !== undefined && <span>Provider full-capacity estimate: <time dateTime={new Date(estimate).toISOString()} title={new Date(estimate).toLocaleString()}>{relative(estimate)}</time>{estimate <= now ? ' (past estimate)' : ''}. <small>{new Date(estimate).toLocaleString()}</small></span>}
      <span>{wait.paused ? 'Retained recheck' : 'Next quota recheck'}: <time dateTime={wait.retry_not_before} title={new Date(retry).toLocaleString()}>{relative(retry)}</time>{retry <= now ? ' (awaiting recheck)' : ''}. <small>{new Date(retry).toLocaleString()}</small></span>
      {estimate !== undefined && <span>The provider estimate does not establish the next available slot, refill pattern or review start. {estimate <= now ? 'The retained estimate is in the past; current quota has not been confirmed.' : 'Actual availability must be rechecked.'}</span>}
    </span>
  </span>;
}
