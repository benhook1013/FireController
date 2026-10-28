import type { ReactNode } from 'react';
import './review-queue.css';

// These shells receive presentation only. Reads, authority and navigation belong
// to the private workspace or the separately approved public projection.
export function ReviewRowDisplay({ id, lifecycle, position, positionLabel, rail = true, number, title, url, className = '', facts, children, footer }: {
  id?: string;
  lifecycle: 'unknown' | 'open' | 'closed' | 'merged';
  position?: string;
  positionLabel?: string;
  rail?: boolean;
  number: number;
  title: string;
  url?: string;
  className?: string;
  facts?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
}) {
  return <ReviewRowFrame id={id} lifecycle={lifecycle} position={position} positionLabel={positionLabel} rail={rail} className={className} facts={facts} footer={footer}
    heading={<div className="review-pull-heading"><span className="review-number">{url ? <a href={url} target="_blank" rel="noopener noreferrer">#{number}</a> : <>#{number}</>}</span><h3>{url ? <a href={url} target="_blank" rel="noopener noreferrer">{title}</a> : title}</h3></div>}>
    {children}
  </ReviewRowFrame>;
}

/** Shared geometry for populated rows and their decorative loading placeholders. */
export function ReviewRowFrame({ id, lifecycle, position, positionLabel, rail = true, className = '', heading, facts, children, footer }: {
  id?: string;
  lifecycle: 'unknown' | 'open' | 'closed' | 'merged';
  position?: string;
  positionLabel?: string;
  rail?: boolean;
  className?: string;
  heading: ReactNode;
  facts?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
}) {
  return <li id={id} data-reading-anchor={id} className={`review-queue-row review-row-${lifecycle}${rail ? '' : ' review-row-without-order'} ${className}`}>
    {rail && <span className="review-queue-position" aria-label={positionLabel}>{position}</span>}
    <div className="review-queue-content">
      {heading}
      {facts}{children}{footer}
    </div>
  </li>;
}

export function ReviewLaneDisplay({ title, label = typeof title === 'string' ? title : undefined, metadata, metadataTitle, status, children, readingAnchor, className = '', channel }: {
  title: ReactNode;
  label?: string;
  metadata: ReactNode;
  metadataTitle?: string;
  status?: ReactNode;
  children: ReactNode;
  readingAnchor?: string;
  className?: string;
  channel?: 'hosted' | 'cli';
}) {
  return <section className={className ? `review-lane ${className}` : 'review-lane'} data-channel={channel} data-reading-anchor={readingAnchor} aria-label={label}><div className="review-lane-heading"><h4>{title}{status}</h4><span title={metadataTitle}>{metadata}</span></div>{children}</section>;
}

type RoundPillContent = { className: string; title: string; 'aria-label': string; children: ReactNode };
export function ReviewRoundPillDisplay({ found, accepted, routed, className = '', zeroAccepted = false, title, label, wrap, children }: {
  found: number;
  accepted: number;
  routed: number;
  className?: string;
  zeroAccepted?: boolean;
  title: string;
  label: string;
  wrap?: (content: RoundPillContent) => ReactNode;
  children: ReactNode;
}) {
  const content: RoundPillContent = {
    className: ['review-round-pill', className.trim(), zeroAccepted ? 'review-pill-zero' : ''].filter(Boolean).join(' '),
    title, 'aria-label': label,
    children: <><strong>{found}/{accepted}/{routed}</strong>{children}</>,
  };
  return wrap ? <>{wrap(content)}</> : <span {...content} />;
}
