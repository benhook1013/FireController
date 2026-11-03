import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router';
import { ApiError } from './api';
import { ActionNotice, useBrowserAction } from './browser-actions';
import { ReadState, signalFor, useWorkspace } from './workspace';
import type { Worker, WorkerRetirement } from './types';

export function RetirementBlockers({ value, home }: { value: WorkerRetirement; home: string }) {
  const selected = encodeURIComponent(value.worker_id);
  return <><p>{value.retired ? 'Retired. Existing history is retained.' : value.can_retire ? 'No unfinished jobs, pending notes or unacknowledged messages block retirement.' : 'Reassign or close unfinished jobs, resolve or reassign pending notes, and acknowledge incoming messages before retiring this worker.'}</p>
    <ul>{(['jobs', 'notes', 'messages'] as const).map(kind => {
      const group = value.blockers[kind];
      const href = kind === 'jobs' ? `${home}/jobs?worker_id=${selected}&status=open` : kind === 'notes' ? `${home}/notes?worker_id=${selected}&include_assigned_jobs=true` : `${home}/inbox?worker_id=${selected}`;
      return <li key={kind}><Link to={href}>{group.count} {kind === 'jobs' ? 'unfinished jobs' : kind === 'notes' ? 'pending notes' : 'unacknowledged messages'}</Link>{group.ids.length > 0 && <ul>{group.ids.map(id => <li key={id}>{kind === 'messages' ? <code>{id}</code> : <Link to={`${home}/${kind}/${encodeURIComponent(id)}`}>{id}</Link>}</li>)}</ul>}{group.count > group.ids.length && <small>Showing {group.ids.length} of {group.count}.</small>}</li>;
    })}</ul>
  </>;
}

// Invalidate only the selected connection's current views; history stays retained.
export async function refreshRetirementViews(client: ReturnType<typeof useQueryClient>, key: readonly string[], workerId: string) {
  for (const suffix of [['workers'], ['delivery'], ['worker', workerId], ['worker-retirement', workerId]]) await client.invalidateQueries({ queryKey: [...key, ...suffix] });
}
export function WorkerRetirementPanel({ worker }: { worker: Worker }) {
  const { session, key, home } = useWorkspace();
  const client = useQueryClient();
  const preview = useQuery({ queryKey: [...key, 'worker-retirement', worker.id], queryFn: ({ signal }) => session.api.workerRetirement(worker.id, signalFor(session, signal)), staleTime: 0 });
  const action = useBrowserAction<{ worker: string; actor: string }, unknown>(JSON.stringify(['retire', worker.id]), async (input, actionKey, signal) => {
    try { return await session.api.retireWorker(input.worker, input.actor, actionKey, signal); }
    catch (error) { if (error instanceof ApiError && error.status === 409) await preview.refetch(); throw error; }
  }, async () => { await refreshRetirementViews(client, key, worker.id); });
  const canWrite = session.api.browserCapabilities.allow_writes;
  return <section className="panel" aria-label={`Retirement for ${worker.name}`}><h3>Worker retirement</h3>
    <ReadState error={preview.error} loading={preview.isPending} empty={false}>{preview.data && <><RetirementBlockers value={preview.data} home={home} /><p>Retirement hides this worker from active choices. It does not erase or reassign work, acknowledge messages, or revoke credentials.</p>
      {canWrite && (action.locked || !preview.data.retired) && <button type="button" disabled={action.busy || !action.locked && !preview.data.can_retire} onClick={() => void action.run({ worker: worker.id, actor: session.worker?.id ?? '' })}>{action.busy ? 'Retiring…' : action.locked ? 'Retry exact retirement' : `Retire ${worker.name}`}</button>}
      <button type="button" disabled={preview.isFetching} onClick={() => void preview.refetch()}>Refresh retirement checks</button>
      {canWrite && preview.data.blockers.messages.ids.length > 0 && <div><p>Project managers may explicitly acknowledge each message on behalf of this worker. This is separate from retirement.</p>{preview.data.blockers.messages.ids.map(id => <RetirementAcknowledgement key={id} workerId={worker.id} messageId={id} refresh={() => preview.refetch().then(() => undefined)} />)}</div>}
    </>}</ReadState>
    <ActionNotice error={action.error instanceof ApiError && action.error.code === 'worker_retirement_blocked' ? undefined : action.error} done={action.done}>Worker retired. History is retained.</ActionNotice>
    {action.error instanceof ApiError && action.error.code === 'worker_retirement_blocked' && <p className="notice" role="alert">Retirement is blocked. Resolve the work listed in the current checks, then try again.</p>}
    {action.retained?.uncertain && <p>Retirement recovery key: <code>{action.retained.key}</code></p>}
  </section>;
}
function RetirementAcknowledgement({ workerId, messageId, refresh }: { workerId: string; messageId: string; refresh(): Promise<void> }) {
  const { session, key } = useWorkspace();
  const client = useQueryClient();
  const action = useBrowserAction<{ actor: string; worker: string; message: string }, unknown>(JSON.stringify(['retirement-ack', workerId, messageId]),
    (input, actionKey, signal) => session.api.acknowledgeWorkerMessage(input.actor, input.worker, input.message, actionKey, signal),
    async () => { await client.invalidateQueries({ queryKey: [...key, 'inbox'] }); await refresh(); });
  return <div><button type="button" disabled={action.busy || action.done} onClick={() => void action.run({ actor: session.worker?.id ?? '', worker: workerId, message: messageId })}>{action.busy ? 'Acknowledging…' : action.locked ? `Retry exact acknowledgement ${messageId}` : `Acknowledge ${messageId} for this worker`}</button><ActionNotice error={action.error} done={action.done}>Message acknowledged.</ActionNotice></div>;
}
