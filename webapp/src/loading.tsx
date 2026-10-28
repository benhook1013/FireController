import './delivery.css';
import { ReviewLaneDisplay, ReviewRowFrame } from './review-display';

/** Neutral placeholders never reuse labels or content from an unverified session. */
export function PageSkeleton({ rows = 3, label = 'Loading content' }: { rows?: number; label?: string }) {
  return <div className="page-skeleton" role="status"><span className="visually-hidden">{label}</span><div aria-hidden="true">{Array.from({ length: rows }, (_, index) => <div className="skeleton-row" key={index}><span className="skeleton-line skeleton-short" /><span className="skeleton-line" /><span className="skeleton-line skeleton-medium" /></div>)}</div></div>;
}

/** The same bounded pill geometry as the eventual channel history. */
export function ReviewRoundsSkeleton({ count, decorative = false }: { count: number; decorative?: boolean }) {
  return <div {...(decorative ? {} : { role: 'status' })}>{!decorative && <span className="visually-hidden">Loading retained rounds</span>}<div className="review-round-pills review-recent-rounds" aria-hidden="true">{Array.from({ length: Math.min(6, Math.max(1, count)) }, (_, index) => <span key={index} className="review-round-pill review-round-placeholder"><strong>&nbsp;</strong><span className="review-pill-age">&nbsp;</span></span>)}</div></div>;
}

/** Reserve the public page shell without inventing its identity or sections. */
export function PublicProjectSkeleton({ embedded = false }: { embedded?: boolean }) {
  const Layout = embedded ? 'div' : 'main';
  return <div className="public-project public-project-skeleton">{!embedded && <header className="mast" aria-hidden="true"><div className="mast-inner"><span className="skeleton-line skeleton-public-brand" /><span className="skeleton-line skeleton-public-mast" /></div></header>}<Layout className="public-layout" role="status"><span className="visually-hidden">Loading published project</span><div aria-hidden="true"><div className="public-hero"><div><span className="skeleton-line skeleton-public-eyebrow" /><span className="skeleton-line skeleton-public-title" /><span className="skeleton-line skeleton-medium" /><span className="skeleton-line skeleton-short" /><span className="skeleton-line skeleton-public-link" /></div><div className="public-hero-status"><span className="skeleton-line skeleton-public-pill" /><span className="skeleton-line skeleton-public-date" /><span className="skeleton-line skeleton-public-action" /></div></div><div className="public-navigation"><span className="skeleton-line" /><span className="skeleton-line" /><span className="skeleton-line" /></div><div className="public-skeleton-content"><span className="skeleton-line skeleton-title" /><span className="skeleton-line" /><span className="skeleton-line" /><span className="skeleton-line skeleton-medium" /></div></div></Layout></div>;
}

export function PublicHistorySkeleton() {
  return <div className="public-history-skeleton" role="status"><span className="visually-hidden">Loading published history</span><div aria-hidden="true"><span className="skeleton-line skeleton-medium" /></div></div>;
}

/** Match the populated PR frame, facts, policies and bounded channel rounds. */
export function MergeHeadSkeleton({ decorative = false }: { decorative?: boolean }) {
  const ink = (className: string) => <span className={`skeleton-ink ${className}`}>&nbsp;</span>;
  const heading = <div className="review-pull-heading"><span className="review-number">{ink('skeleton-pr-number')}</span><h3 className="skeleton-pr-title">{ink('skeleton-title-first')}{ink('skeleton-title-second')}{ink('skeleton-title-third')}{ink('skeleton-title-fourth')}</h3></div>;
  const facts = <div className="review-pull-facts"><span className="badge skeleton-fact skeleton-fact-state">{ink('')}</span>{['diff','ci','merge'].map(kind => <span key={kind} className={`badge review-fact-pill skeleton-fact skeleton-fact-${kind}`}>{ink('')}</span>)}</div>;
  return <div className="delivery-head-skeleton" {...(decorative ? {} : { role: 'status' })}>{!decorative && <span className="visually-hidden">Loading merge head</span>}<div className="delivery-head-placeholder" aria-hidden="true">
    <p className="eyebrow">{ink('skeleton-pr-eyebrow')}</p>
    <ul className="review-queue"><ReviewRowFrame lifecycle="unknown" rail={false} className="review-row-loading" heading={heading} facts={facts} footer={<span className="review-details-link skeleton-pr-link">{ink('')}</span>}>
      <section className="review-history"><div className="review-lanes">{(['hosted','cli'] as const).map(channel => <ReviewLaneDisplay key={channel} channel={channel} className="skeleton-review-lane" title={ink('skeleton-channel-title')} metadata={ink('skeleton-channel-total')}>
        <div className="review-current-progress">{ink('skeleton-channel-policy')}</div><ReviewRoundsSkeleton count={6} decorative />
      </ReviewLaneDisplay>)}</div></section>
    </ReviewRowFrame></ul>
  </div></div>;
}

export function DeliveryHeroSkeleton() {
  return <div className="delivery-hero skeleton-hero"><section className="hero project-identity"><div className="hero-project"><h1 className="skeleton-identity-title"><span className="skeleton-ink">&nbsp;</span></h1><p className="repository skeleton-identity-repository"><span className="skeleton-ink">&nbsp;</span></p></div></section><section className="hero delivery-merge-head"><MergeHeadSkeleton decorative /></section></div>;
}

/** Reserve the actual header controls without claiming connection or showing actions. */
export function HeaderUtilitiesSkeleton() {
  return <div className="header-utilities header-utilities-skeleton" aria-hidden="true"><span className="connection-note"><span className="skeleton-ink">&nbsp;</span></span><span className="skeleton-utility-button"><span className="skeleton-ink">&nbsp;</span></span><span className="skeleton-utility-button"><span className="skeleton-ink">&nbsp;</span></span></div>;
}

export function WorkspaceSkeleton({ sidebar = false }: { sidebar?: boolean }) {
  return <main className="workspace workspace-skeleton" role="status"><span className="visually-hidden">Opening project</span><div aria-hidden="true"><DeliveryHeroSkeleton /><div className="skeleton-navigation"><span className="skeleton-line" /></div><div className={sidebar ? 'workspace-grid' : 'project-main'}>{sidebar && <aside className="sidebar"><div className="panel skeleton-sidebar"><span className="skeleton-line skeleton-medium" /><span className="skeleton-line" /><span className="skeleton-line skeleton-short" /></div></aside>}<div className="main-content"><div className="skeleton-heading"><span className="skeleton-line skeleton-title" /><span className="skeleton-line skeleton-short" /></div><div className="panel"><PageSkeleton /></div></div></div></div></main>;
}
