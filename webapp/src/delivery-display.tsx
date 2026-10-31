import type { ReactNode } from 'react';
import './delivery.css';

export function WorkerLaneDisplay({ label, name, ariaLabel = `${label} jobs`, presence, links, status, details, children, readingAnchor }: {
  label: string; name: ReactNode; ariaLabel?: string; presence?: ReactNode; links?: ReactNode;
  status?: ReactNode; details?: ReactNode; children: ReactNode;
  readingAnchor?: string;
}) {
  return <section className="panel delivery-lane" aria-label={ariaLabel} data-reading-anchor={readingAnchor}>
    <header className="delivery-lane-heading"><div className="delivery-worker-heading"><h3>{name}{presence}</h3>{status}</div>
      {links && <nav className="delivery-worker-links" aria-label={`${label} links`}>{links}</nav>}
      {details}
    </header>
    {children}
  </section>;
}

export function DeliveryJobCard({ title, status, children, footer, readingAnchor }: {
  title?: ReactNode; status?: ReactNode; children?: ReactNode; footer?: ReactNode;
  readingAnchor?: string;
}) {
  return <li data-reading-anchor={readingAnchor}>
    {(title || status) && <div className="delivery-job-heading">{status && <div className="delivery-job-state">{status}</div>}{title && <h4>{title}</h4>}</div>}
    {children}{footer}
  </li>;
}
