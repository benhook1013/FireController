import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import type { ReviewAttemptStatus } from './review-attempt-status';

export function ReviewAttemptBadge({ channel, status }: { channel: string; status: ReviewAttemptStatus }) {
  const className = status.kind ? `badge review-channel-position review-attempt-state review-attempt-${status.kind}` : 'badge review-channel-position';
  if (!status.activity) return <span className={className} title={status.description} aria-label={status.kind ? `${channel} execution: ${status.text}. ${status.description ?? ''}` : undefined}>{status.text}</span>;
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
  const stamp = (value: string) => <><time dateTime={value}>{new Date(value).toLocaleString(undefined, { timeZoneName: 'short' })}</time><small>{value}</small></>;
  return <span ref={wrapper} className="review-activity" onPointerEnter={event => { cancelClose(); if (event.pointerType === 'mouse') setExpanded(true); }} onPointerLeave={event => { if (event.pointerType === 'mouse' && !event.currentTarget.contains(document.activeElement)) closeTimer.current = setTimeout(() => setExpanded(false), 150); }} onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) setExpanded(false); }} onKeyDown={event => { if (event.key === 'Escape') { event.stopPropagation(); setExpanded(false); } }}>
    <span ref={trigger} role="button" tabIndex={0} className="badge review-channel-position review-activity-label" aria-label={`${channel} execution: ${status.text}`} aria-describedby={detailsID} aria-controls={detailsID} aria-expanded={expanded} onFocus={event => { if (event.currentTarget.matches(':focus-visible')) setExpanded(true); }} onClick={event => { event.stopPropagation(); setExpanded(true); }} onKeyDown={event => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); event.stopPropagation(); setExpanded(value => !value); } }}>{status.text}</span>
    <span ref={details} id={detailsID} className="review-activity-details" style={placement} hidden={!expanded} role="tooltip">
      {activity.evidence?.kind === 'cli_process_spawned' && <span>CLI process started: {stamp(activity.evidence.process_started_at)}</span>}
      {activity.evidence?.kind === 'hosted_processing_observed' && <><span>CodeRabbit processing observed: {stamp(activity.evidence.observed_at)}</span><span>Provider comment updated: {stamp(activity.evidence.comment_updated_at)}</span></>}
      <span>{activity.startedAt ? <>Attempt began: {stamp(activity.startedAt)}</> : 'Attempt start time is unknown.'}</span>
      <span>{activity.explanation}</span>
    </span>
  </span>;
}
