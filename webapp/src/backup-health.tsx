import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import type { Session } from './workspace';
import { backupHeadline, type BackupHealth, type BackupHealthRow } from './backup-health-model';
import { RelativeTime } from './relative-time';

const stateLabels = { healthy: 'Current', running: 'In progress', failed: 'Needs attention', overdue: 'Overdue', unconfigured: 'Not configured', unknown: 'Status unavailable' };
function CopyStatus({ label, row }: { label: string; row: BackupHealthRow }) {
  return <li><strong>{label}</strong><span>{stateLabels[row.state]}</span>{row.last_success_at && <span>Snapshot <RelativeTime value={row.last_success_at} /></span>}{row.state === 'failed' && row.reason?.includes('retention') && <span>Older-copy cleanup needs attention</span>}</li>;
}
export function dismissBackupHealth(details: HTMLDetailsElement) {
  const document = details.ownerDocument;
  const capture = { capture: true };
  const outside = (event: Event) => {
    if (details.open && event.target instanceof Node && !details.contains(event.target)) details.open = false;
  };
  const escape = (event: KeyboardEvent) => {
    if (event.key !== 'Escape' || !details.open) return;
    details.open = false;
    details.querySelector('summary')?.focus();
    event.preventDefault();
  };
  document.addEventListener('pointerdown', outside, capture);
  document.addEventListener('click', outside, capture);
  document.addEventListener('keydown', escape);
  return () => {
    document.removeEventListener('pointerdown', outside, capture);
    document.removeEventListener('click', outside, capture);
    document.removeEventListener('keydown', escape);
  };
}
export function BackupHealthView({ health, unavailable = false, now = Date.now() }: { health?: BackupHealth; unavailable?: boolean; now?: number }) {
  const details = useRef<HTMLDetailsElement>(null);
  useEffect(() => details.current ? dismissBackupHealth(details.current) : undefined, []);
  const headline = backupHeadline(health, unavailable, now);
  const stale = !health || unavailable || now - Date.parse(health.checked_at) > 120000 || Date.parse(health.checked_at) > now + 30000;
  return <details ref={details} className={`backup-health backup-health-${headline.tone}`}><summary><span className="backup-health-dot" aria-hidden="true" />{headline.label}</summary><div className="backup-health-details"><h3 className="backup-health-heading">Backup status</h3>{stale ? <p>Current status could not be checked.</p> : <><ul><CopyStatus label="Local" row={health.local} />{health.offsite.map((row, index) => <CopyStatus key={index} label={row.label} row={row} />)}</ul>{health.offsite.length === 0 && <p>Offsite status reporting is not configured.</p>}<p>Times show the latest successfully backed-up snapshot.</p></>}</div></details>;
}
export function BackupHealthIndicator({ session }: { session: Session }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 30000); return () => clearInterval(timer); }, []);
  const query = useQuery({ queryKey: ['fc', session.nonce, session.instanceId, session.project.id, 'backup-health'], queryFn: ({ signal }) => session.api.backupHealth(signal), staleTime: 30000, refetchInterval: 60000, retry: false });
  return <BackupHealthView health={query.data} unavailable={query.isError} now={now} />;
}
