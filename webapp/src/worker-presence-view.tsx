import { useEffect, useSyncExternalStore } from 'react';
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
export function WorkerContactView({ value, worker }: { value?: WorkerPresence; worker: string }) {
  const item=value?.items.find(item => item.worker_id===worker);
  if (!item || !item.attached && !item.last_seen_at) return null;
  const detail=`Agent client contact observed since ${new Date(value!.observed_since).toLocaleString()}. This does not indicate model activity or job progress.${value!.complete ? '' : ' Some client contact could not be retained.'}`;
  return <span className="worker-contact" title={detail}>{item.attached ? 'Client attached' : <>Last contact <RelativeTime value={item.last_seen_at!}/></>}</span>;
}
