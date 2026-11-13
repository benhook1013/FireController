import { useState } from 'react';
import type { FormEvent } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ApiError } from './api';
import { ActionNotice, useBrowserAction } from './browser-actions';
import { useWorkspace } from './workspace';
import type { Worker } from './types';

type RenameInput = { actor: string; expectedName: string; name: string };

export function WorkerNameEditor({ worker }: { worker: Worker }) {
  const { session, key } = useWorkspace();
  const client = useQueryClient();
  const [draft, setDraft] = useState<{ expectedName: string; name: string }>();
  const detailKey = [...key, 'worker', worker.id];
  const action = useBrowserAction<RenameInput, Pick<Worker, 'id' | 'name'>>(
    JSON.stringify(['rename-worker', worker.id]),
    (input, actionKey, signal) => session.api.renameWorker(worker.id, input.actor, input.expectedName, input.name, actionKey, signal),
    async () => {
      setDraft(undefined);
      await Promise.all([
        client.invalidateQueries({ queryKey: detailKey }),
        client.invalidateQueries({ queryKey: [...key, 'workers'] }),
        client.invalidateQueries({ queryKey: [...key, 'delivery'] }),
      ]);
    },
  );
  const input = action.retained?.input ?? draft;
  const refusal = action.error instanceof ApiError && !action.error.outcomeUnknown ? action.error.code : '';
  const stale = refusal === 'worker_name_changed' || Boolean(draft && draft.expectedName !== worker.name);
  function submit(event: FormEvent) {
    event.preventDefault();
    if (!input) return;
    void action.run({ ...input, name: input.name.trim(), actor: session.worker?.id ?? '' });
  }
  async function useCurrentName() {
    await client.invalidateQueries({ queryKey: detailKey });
    const current = client.getQueryData<Worker>(detailKey);
    if (!current) return;
    setDraft(draft => ({ expectedName: current.name, name: draft?.name ?? current.name }));
    action.clearNotice();
  }

  return <>
    {!input && <button type="button" onClick={() => { action.clearNotice(); setDraft({ expectedName: worker.name, name: worker.name }); }}>Rename worker</button>}
    {input && <form className="browser-action-form" data-browser-draft data-browser-dirty={input.name !== input.expectedName} data-browser-busy={action.busy} onSubmit={submit}>
      <label>Display name<input autoFocus value={input.name} disabled={action.locked} required maxLength={200} onChange={event => { action.clearNotice(); setDraft({ expectedName: input.expectedName, name: event.target.value }); }} /></label>
      {stale && !action.locked && <p className="notice" role="alert">The worker name changed. Reload the current name before saving your edit.</p>}
      {refusal === 'worker_already_configured' && <p className="notice" role="alert">A worker with this name already exists in this project.</p>}
      <div className="action-buttons">
        {stale && !action.locked ? <button type="button" onClick={() => void useCurrentName()}>Reload current name</button> :
          <button className="primary" type="submit" disabled={action.busy || !action.locked && (!input.name.trim() || input.name.trim() === input.expectedName)}>{action.busy ? 'Saving…' : action.locked ? 'Retry rename' : 'Save name'}</button>}
        {!action.locked && <button type="button" onClick={() => { setDraft(undefined); action.clearNotice(); }}>Cancel</button>}
      </div>
    </form>}
    <ActionNotice error={stale || refusal === 'worker_already_configured' ? undefined : action.error} done={action.done}>Worker renamed.</ActionNotice>
  </>;
}
