import type { ReactNode } from 'react';
import './delivery.css';

export function WorkerLaneDisplay({ label, name, ariaLabel = `${label} jobs`, presence, links, status, details, children }: {
  label: string; name: ReactNode; ariaLabel?: string; presence?: ReactNode; links?: ReactNode;
  status?: ReactNode; details?: ReactNode; children: ReactNode;
}) {
  return <section className="panel delivery-lane" aria-label={ariaLabel}>
    <header className="delivery-lane-heading"><h3>{name}{presence}</h3>
      {links && <nav className="delivery-worker-links" aria-label={`${label} links`}>{links}</nav>}
      {status}{details}
    </header>
    {children}
  </section>;
}

export function DeliveryJobCard({ title, status, children, footer }: {
  title?: ReactNode; status?: ReactNode; children?: ReactNode; footer?: ReactNode;
}) {
  return <li>
    {(title || status) && <div className="delivery-job-heading">{title && <h4>{title}</h4>}{status && <div className="delivery-job-state">{status}</div>}</div>}
    {children}{footer}
  </li>;
}
