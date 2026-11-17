import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import type { QueueSorting, QueueSortingReceipt } from './api';
import { ActionNotice, useBrowserAction } from './browser-actions';
import { ReadState, signalFor, useWorkspace } from './workspace';

type Input = { worker: string; revision: number; mode: 'auto'|'manual' };
export function QueueSortingSettings() {
 const {session,key}=useWorkspace();const [reload,setReload]=useState(0);
 const query=useQuery({queryKey:[...key,'queue-sorting'],queryFn:({signal})=>session.api.queueSorting(signalFor(session,signal)),staleTime:Infinity});
 return <section className="panel"><h3>Queue sorting</h3><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <QueueSortingEditor key={reload} initial={query.data} reload={async()=>{const result=await query.refetch();if(!result.error)setReload(value=>value+1);}} />}</ReadState></section>;
}
function QueueSortingEditor({initial,reload}:{initial:QueueSorting;reload():Promise<void>}) {
 const {session,key}=useWorkspace();const client=useQueryClient();const worker=session.worker?.id ?? '';
 const slot=JSON.stringify([worker,'queue-sorting']);const prior=session.actions.get<Input>(slot)?.input;
 const [mode,setMode]=useState<'auto'|'manual'>(prior?.mode ?? initial.mode);
 const [revision,setRevision]=useState(prior?.revision ?? initial.revision);
 const action=useBrowserAction<Input,QueueSortingReceipt>(slot,(input,key,signal)=>session.api.setQueueSorting(input.worker,input.revision,input.mode,key,signal),async receipt=>{setRevision(receipt.revision);await client.invalidateQueries({queryKey:key});});
 return <form className="browser-action-form" onSubmit={event=>{event.preventDefault();void action.run({worker,revision,mode});}}>
 <label>Project queue order<select aria-label="Project queue order" value={mode} disabled={action.locked || !initial.writable} onChange={event=>{setMode(event.target.value as 'auto'|'manual');action.clearNotice();}}><option value="auto">Auto</option><option value="manual">Manual</option></select></label>
 {!initial.writable && <p className="notice">Sorting changes are temporarily unavailable until compatible backup and rollback readers are installed. The displayed mode remains effective.</p>}
 <p>Auto puts proven independent PRs first and keeps stack parents before children. Draft status does not change priority. Unresolved dependencies retain their saved positions.</p>
 <p>Manual uses saved queue order. Switching modes never rewrites it. Both modes retain all review eligibility and dependency checks.</p>
 <button type="submit" disabled={!worker || action.busy || !initial.writable}>{action.busy?'Saving…':action.locked?'Retry exact sorting save':'Save queue sorting'}</button>
 <button type="button" disabled={action.locked} onClick={()=>{void reload();}}>Reload current sorting</button>
 <ActionNotice error={action.error} done={action.done}>Queue sorting saved.</ActionNotice>
 </form>;
}
