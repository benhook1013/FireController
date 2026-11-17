import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import type { ReviewAttemptStatus } from './review-attempt-status';

export function ReviewAttemptBadge({ channel, status }: { channel: string; status: ReviewAttemptStatus }) {
  const className = status.kind ? `badge review-channel-position review-attempt-state review-attempt-${status.kind}` : 'badge review-channel-position';
  if (!status.activity) {
    const cause = status.cause && !/[.!?]$/u.test(status.cause) ? `${status.cause}.` : status.cause;
    const description = [cause, status.description].filter(Boolean).join(' ') || undefined;
    return <span className={className} title={description} aria-label={status.kind ? `${channel} execution: ${status.text}. ${description ?? ''}` : undefined}>{status.text}</span>;
  }
  return <ActivityBadge key={JSON.stringify([channel, status.text, status.activity])} channel={channel} status={status} />;
}

function ActivityBadge({ channel, status }: { channel: string; status: ReviewAttemptStatus }) {
  const [expanded, setExpanded] = useState(false);
  const [placement, setPlacement] = useState({ left: 8, top: 8, width: 360 });
  const wrapper = useRef<HTMLSpanElement>(null);
  const trigger = useRef<HTMLSpanElement>(null);
  const details = useRef<HTMLSpanElement>(null);
  const closeTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const cancelClose = () => { if (closeTimer.current !== undefined) clearTimeout(closeTimer.current); };
  const detailsID = useId();
  useEffect(() => () => cancelClose(), []);
  useLayoutEffect(() => {
    if (!expanded) return;
    const place = () => {
      const rect = trigger.current?.getBoundingClientRect();
      if (!rect) return;
      const width = Math.max(0, Math.min(360, window.innerWidth - 16));
      const height = details.current?.getBoundingClientRect().height ?? 0;
      const below = rect.bottom + 4;
      const top = below + height <= window.innerHeight - 8 ? below : Math.max(8, rect.top - height - 4);
      setPlacement({ left: Math.max(8, Math.min(rect.left, window.innerWidth - width - 8)), top, width });
    };
    place(); window.addEventListener('resize', place); window.addEventListener('scroll', place, true);
    return () => { window.removeEventListener('resize', place); window.removeEventListener('scroll', place, true); };
  }, [expanded, placement.width]);
  useEffect(() => {
    if (!expanded) return;
    const outside = (event: PointerEvent) => { if (event.target instanceof Node && !wrapper.current?.contains(event.target)) setExpanded(false); };
    document.addEventListener('pointerdown', outside);
    return () => document.removeEventListener('pointerdown', outside);
  }, [expanded]);
  const activity = status.activity!;
  const evidence = activity.evidence;
  const timing = evidence?.kind === 'cli_process_spawned'
    ? { label: 'CLI started', value: evidence.process_started_at }
    : evidence?.kind === 'hosted_processing_observed'
      ? { label: 'CodeRabbit activity seen', value: evidence.observed_at }
      : activity.startedAt ? { label: 'Preparation began', value: activity.startedAt } : undefined;
  return <span ref={wrapper} className="review-activity" onPointerEnter={event => { cancelClose(); if (event.pointerType === 'mouse') setExpanded(true); }} onPointerLeave={event => { if (event.pointerType === 'mouse' && !event.currentTarget.contains(document.activeElement)) closeTimer.current = setTimeout(() => setExpanded(false), 150); }} onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) setExpanded(false); }} onKeyDown={event => { if (event.key === 'Escape') { event.stopPropagation(); setExpanded(false); } }}>
    <span ref={trigger} role="button" tabIndex={0} className="badge review-channel-position review-activity-label" aria-label={`${channel} execution: ${status.text}`} aria-describedby={detailsID} aria-controls={detailsID} aria-expanded={expanded} onFocus={event => { if (event.currentTarget.matches(':focus-visible')) setExpanded(true); }} onClick={event => { event.stopPropagation(); setExpanded(true); }} onKeyDown={event => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); event.stopPropagation(); setExpanded(value => !value); } }}>{status.text}</span>
    <span ref={details} id={detailsID} className="review-activity-details" style={placement} hidden={!expanded} role="tooltip">
      <span>{activity.explanation}</span>
      {timing && <span>{timing.label}: <time dateTime={timing.value}>{new Date(timing.value).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })}</time></span>}
    </span>
  </span>;
}
