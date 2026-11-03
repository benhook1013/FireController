import { useEffect, useRef, useSyncExternalStore } from 'react';
import { useQuery } from '@tanstack/react-query';
import { signalFor, useWorkspace } from './workspace';
import { RelativeTime } from './relative-time';
import { presenceSelection, presenceWorkers, type WorkerPresence } from './worker-presence';

const visibility = (notify: () => void) => { document.addEventListener('visibilitychange',notify); return () => document.removeEventListener('visibilitychange',notify); };
const visible = () => document.visibilityState !== 'hidden';
export function useWorkerPresence(workers: string[]) {
  const { session, key, fail } = useWorkspace();
  const selected = presenceSelection(workers);
  const foreground = useSyncExternalStore(visibility,visible,() => false);
  const query = useQuery({queryKey:[...key,'worker-presence',selected],
    queryFn:({signal}) => session.api.workerPresence(selected,signalFor(session,signal)),
    enabled:foreground && presenceWorkers(selected), staleTime:15000,
    refetchInterval:foreground ? 15000 : false, refetchIntervalInBackground:false});
  useEffect(() => { if (query.error) fail(query.error); },[query.error,fail]);
  return query;
}
export function dismissWorkerPresence(details: HTMLDetailsElement) {
  const document = details.ownerDocument;
  const capture = { capture: true };
  const outside = (event: Event) => {
    if (details.open && event.target instanceof Node && !details.contains(event.target)) details.open = false;
  };
  const escape = (event: KeyboardEvent) => {
    if (event.key !== 'Escape' || !details.open) return;
    details.querySelector('summary')?.focus();
    details.open = false;
    event.preventDefault();
  };
  document.addEventListener('pointerdown', outside, capture);
  document.addEventListener('click', outside, capture);
  document.addEventListener('keydown', escape);
  document.addEventListener('focusin', outside);
  return () => {
    document.removeEventListener('pointerdown', outside, capture);
    document.removeEventListener('click', outside, capture);
    document.removeEventListener('keydown', escape);
    document.removeEventListener('focusin', outside);
  };
}
export function WorkerPresenceView({ value, worker, name, paused = false, reason = '', reasonTruncated = false }: {
  value?: WorkerPresence; worker: string; name: string; paused?: boolean; reason?: string; reasonTruncated?: boolean;
}) {
  const details = useRef<HTMLDetailsElement>(null);
  useEffect(() => details.current ? dismissWorkerPresence(details.current) : undefined, []);
  const item = value?.items.find(item => item.worker_id === worker);
  const known = !!item && (item.attached || !!item.last_seen_at);
  const state = paused ? 'paused' : known ? 'active' : 'unknown';
  const label = paused ? 'Paused' : known ? 'Active' : 'Contact unknown';
  return <details ref={details} className={`worker-presence worker-presence-${state}`}
    onMouseEnter={event => { if (window.matchMedia('(hover: hover)').matches) event.currentTarget.open = true; }}
    onMouseLeave={event => { if (!event.currentTarget.contains(document.activeElement)) event.currentTarget.open = false; }}>
    <summary aria-label={`${name}: ${label}`} onFocus={event => { if (event.currentTarget.matches(':focus-visible')) (event.currentTarget.parentElement as HTMLDetailsElement).open = true; }}>
      {paused ? 'Paused' : known ? 'Active' : 'Unknown'}
    </summary>
    <div className="worker-presence-details"><strong>{label}</strong>
      {paused && <p className="worker-presence-reason">{reason ? reason + (reasonTruncated ? '…' : '') : 'This worker is explicitly paused.'}</p>}
      {item?.attached && <p>Client attached</p>}
      {item?.last_seen_at && <p>Last contact <RelativeTime value={item.last_seen_at}/></p>}
      {!known && <p>Client contact is unknown. Contact observations clear when the controller restarts; unknown does not mean this worker is stopped or paused.</p>}
      <p>Client contact does not indicate model activity or job progress.</p>
      {value && <p>Contact observations cover the period since {new Date(value.observed_since).toLocaleString()}.{value.complete ? '' : ' Some client contact could not be retained.'}</p>}
    </div>
  </details>;
}
