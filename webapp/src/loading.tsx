import './delivery.css';

/** Neutral placeholders never reuse labels or content from an unverified session. */
export function PageSkeleton({ rows = 3, label = 'Loading content' }: { rows?: number; label?: string }) {
  return <div className="page-skeleton" role="status"><span className="visually-hidden">{label}</span><div aria-hidden="true">{Array.from({ length: rows }, (_, index) => <div className="skeleton-row" key={index}><span className="skeleton-line skeleton-short" /><span className="skeleton-line" /><span className="skeleton-line skeleton-medium" /></div>)}</div></div>;
}

/** Match the joined hero without guessing a project name or review result. */
export function MergeHeadSkeleton({ decorative = false }: { decorative?: boolean }) {
  return <div className="delivery-head-skeleton" {...(decorative ? {} : { role: 'status' })}>{!decorative && <span className="visually-hidden">Loading merge head</span>}<div className="delivery-head-placeholder" aria-hidden="true"><span className="skeleton-line skeleton-eyebrow" /><span className="skeleton-line skeleton-head-title" /><div className="skeleton-head-facts"><span className="skeleton-line" /><span className="skeleton-line" /><span className="skeleton-line" /></div><div className="delivery-head-history"><div className="review-lanes">{['hosted', 'cli'].map(channel => <div className="review-lane skeleton-review-lane" key={channel} data-channel={channel}><div className="review-lane-heading"><span className="skeleton-line" /><span className="skeleton-line" /></div><div className="skeleton-rounds"><span className="skeleton-round" /><span className="skeleton-round" /><span className="skeleton-round" /></div></div>)}</div></div><span className="skeleton-line skeleton-head-link" /></div></div>;
}

export function DeliveryHeroSkeleton() {
  return <div className="delivery-hero skeleton-hero"><section className="hero project-identity"><div className="hero-project"><span className="skeleton-line skeleton-title" /><span className="skeleton-line skeleton-medium" /></div></section><section className="hero delivery-merge-head"><MergeHeadSkeleton decorative /></section></div>;
}

export function WorkspaceSkeleton({ sidebar = false }: { sidebar?: boolean }) {
  return <main className="workspace workspace-skeleton" role="status"><span className="visually-hidden">Opening project</span><div aria-hidden="true"><DeliveryHeroSkeleton /><div className="skeleton-navigation"><span className="skeleton-line" /></div><div className={sidebar ? 'workspace-grid' : 'project-main'}>{sidebar && <aside className="sidebar"><div className="panel skeleton-sidebar"><span className="skeleton-line skeleton-medium" /><span className="skeleton-line" /><span className="skeleton-line skeleton-short" /></div></aside>}<div className="main-content"><div className="skeleton-heading"><span className="skeleton-line skeleton-title" /><span className="skeleton-line skeleton-short" /></div><div className="panel"><PageSkeleton /></div></div></div></div></main>;
}
