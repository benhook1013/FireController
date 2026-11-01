import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ActionNotice, useBrowserAction } from './browser-actions';
import { signalFor, useWorkspace } from './workspace';
import type { PullScheduling, ReviewScheduling, SchedulingReceipt, Worker } from './types';
import './review-scheduling.css';

export const reviewSchedulingKey = (key: readonly string[]) => [...key, 'review-scheduling'];

function SchedulingDetails({ label, children, onOpen }: { label: string; children: ReactNode; onOpen?(): void }) {
  const ref=useRef<HTMLDetailsElement>(null);
  useEffect(()=>{
    const details=ref.current;if (!details) return;
    const outside=(event:Event)=>{if (event.target instanceof Node && !details.contains(event.target)) details.open=false;};
    const escape=(event:KeyboardEvent)=>{if (event.key==='Escape' && details.open) {details.open=false;details.querySelector('summary')?.focus();}};
    document.addEventListener('pointerdown',outside,true);document.addEventListener('keydown',escape);
    return ()=>{document.removeEventListener('pointerdown',outside,true);document.removeEventListener('keydown',escape);};
  },[]);
  return <details className="review-scheduling-menu" ref={ref} onToggle={event=>{if(event.currentTarget.open) {const editor=event.currentTarget.querySelector<HTMLElement>('.review-scheduling-editor');if(editor){editor.style.transform='';const bounds=editor.getBoundingClientRect();const offset=bounds.left<8?8-bounds.left:bounds.right>window.innerWidth-8?window.innerWidth-8-bounds.right:0;editor.style.transform=`translateX(${offset}px)`;}onOpen?.();}}}><summary>{label}</summary><div className="review-scheduling-editor">{children}</div></details>;
}

export function ReviewSchedulingBar() {
  const {session,key,ready}=useWorkspace();
  const query=useQuery({queryKey:reviewSchedulingKey(key),queryFn:({signal})=>session.api.reviewScheduling(signalFor(session,signal)),enabled:ready,staleTime:30_000,refetchInterval:60_000,retry:false});
  const value=query.data;
  if (!value) return query.error ? <span className="review-scheduling-status" title="Scheduling settings could not be loaded.">Scheduling unavailable</span> : null;
  const blocked=value.reason==='provider_hold'?'Provider hold':value.reason==='authorizer_unavailable'?'Authorizer unavailable':undefined;
  return <div className="review-scheduling-bar"><span className="review-scheduling-status" title="Automatic Hosted requests only; CLI remains manual.">Automatic Hosted: {value.automatic_hosted?'On':'Off'}</span>{blocked && <span className="review-scheduling-warning" title={value.reason}>{blocked}</span>}
    {session.api.browserCapabilities?.can_edit_presentation && <ProjectSchedulingEditor value={value} reload={async()=>{const result=await query.refetch();return result.data;}} />}
  </div>;
}
function ProjectSchedulingEditor({value,reload}:{value:ReviewScheduling;reload():Promise<ReviewScheduling|undefined>}) {
  const {session,key}=useWorkspace();
  const queryClient=useQueryClient();
  const form=useRef<HTMLFormElement>(null);
  const [choose,setChoose]=useState(false);
  const [worker,setWorker]=useState('');
  const [desired,setDesired]=useState(value.automatic_hosted);
  const [observedRevision,setObservedRevision]=useState(value.revision);
  const [cursor,setCursor]=useState('');
  const [loaded,setLoaded]=useState<Worker[]>([]);
  const selected=session.worker && !session.worker.retired ? session.worker.id : '';
  const workers=useQuery({queryKey:[...key,'scheduling-workers',cursor],queryFn:({signal})=>session.api.workers({limit:100,...(cursor?{cursor}:{})},signalFor(session,signal)),enabled:choose && !selected,staleTime:30_000,retry:false});
  const choices=[...loaded,...(workers.data?.items ?? [])].filter((item,index,items)=>!item.retired && items.findIndex(other=>other.id===item.id)===index);
  const action=useBrowserAction<{worker:string;automatic:boolean;revision:number},SchedulingReceipt>('review-scheduling',
    (input,actionKey,signal)=>session.api.configureReviewScheduling(input.worker,input.automatic,input.revision,actionKey,signal),
    async()=>{await queryClient.invalidateQueries({queryKey:reviewSchedulingKey(key)});});
  useEffect(()=>{if(action.done){const details=form.current?.closest('details');if(details) details.open=false;}},[action.done]);
  const automatic=action.retained?.input.automatic ?? desired;
  const authorizer=action.retained?.input.worker ?? (selected || worker);
  const submit=(event:FormEvent)=>{event.preventDefault();void action.run({worker:automatic?authorizer:'',automatic,revision:observedRevision});};
  const current=()=>{setDesired(value.automatic_hosted);setObservedRevision(value.revision);};
  return <SchedulingDetails label="Scheduling" onOpen={()=>{setChoose(true);if(!action.locked){current();action.clearNotice();}}}><form ref={form} onSubmit={submit}><strong>Review scheduling</strong><label className="review-scheduling-checkbox"><input type="checkbox" checked={automatic} disabled={action.locked} onChange={event=>setDesired(event.target.checked)} />Automatic Hosted reviews</label><p>CLI stays manual. Existing grants and review limits still apply.</p>
    {automatic && !selected && <label>Authorizing worker<select aria-label="Authorizing worker" value={action.retained?.input.worker ?? worker} disabled={action.locked} required onChange={event=>setWorker(event.target.value)}><option value="">Choose an active worker</option>{choices.map(item=><option key={item.id} value={item.id}>{item.name}</option>)}</select></label>}
    {automatic && selected && <p>Authorizing worker: {session.worker?.name}</p>}
    {automatic && !selected && workers.isFetching && <small>Loading workers…</small>}
    {automatic && !selected && workers.error && <p role="alert">Active workers could not be loaded. <button type="button" onClick={()=>void workers.refetch()}>Retry workers</button></p>}
    {automatic && !selected && workers.data?.has_more && <button type="button" disabled={workers.isFetching} onClick={()=>{setLoaded(choices);setCursor(workers.data?.cursor ?? '');}}>Load more workers</button>}
    <button type="submit" disabled={action.busy || automatic && !authorizer || !action.locked && !automatic && !value.automatic_hosted}>{action.busy?'Saving…':action.locked?'Retry exact scheduling change':automatic===value.automatic_hosted?(automatic?'Save authorization':'Save scheduling'):`${automatic?'Enable':'Disable'} automatic Hosted`}</button>
    {Boolean(action.error) && !action.locked && <button type="button" onClick={()=>void reload().then(updated=>{if(updated){setObservedRevision(updated.revision);setDesired(updated.automatic_hosted);}})}>Reload current settings</button>}<ActionNotice error={action.error} done={action.done} />
  </form></SchedulingDetails>;
}

export function ReviewPullSchedulingControl({number}: {number:number}) {
  const {session,key}=useWorkspace();
  const queryClient=useQueryClient();
  // Subscribe to the one project read owned by ReviewSchedulingBar; rows never
  // fetch/poll scheduling independently just to show their paused flags.
  const project=useQuery({queryKey:reviewSchedulingKey(key),queryFn:({signal})=>session.api.reviewScheduling(signalFor(session,signal)),enabled:false,staleTime:Infinity});
  const exact=useQuery({queryKey:[...key,'pull-scheduling',number],queryFn:({signal})=>session.api.pullScheduling(number,signalFor(session,signal)),enabled:false,staleTime:Infinity});
  const paused=project.data?.paused_pulls.find(item=>item.pull_number===number);
  const freshExact=exact.dataUpdatedAt>=project.dataUpdatedAt?exact.data:undefined;
  const value=paused && (!freshExact || paused.revision>=freshExact.revision)?paused:freshExact;
  const writer=session.api.browserCapabilities?.allow_writes===true;
  const [editEpoch,setEditEpoch]=useState(0);
  if (!project.data && !value) return null;
  return <div className="review-pull-scheduling">{value?.paused && <span className="review-scheduling-warning" title={value.reason || 'New Hosted and CLI reviews are paused; issued reviews keep collecting.'}>Reviews paused{value.reason && <span className="review-pause-reason">: {value.reason}</span>}</span>}
    {writer && <SchedulingDetails label={value?.paused?'Resume reviews':'Pause reviews'} onOpen={()=>{setEditEpoch(epoch=>epoch+1);if(!value) void exact.refetch();}}><PullSchedulingEditor key={editEpoch} number={number} value={value} loading={exact.isFetching} error={exact.error} reload={async()=>{const result=await exact.refetch();return result.data;}} changed={async()=>{await queryClient.invalidateQueries({queryKey:reviewSchedulingKey(key)});await exact.refetch();}} /></SchedulingDetails>}
  </div>;
}
function PullSchedulingEditor({number,value,loading,error,reload,changed}:{number:number;value?:PullScheduling;loading:boolean;error:unknown;reload():Promise<PullScheduling|undefined>;changed():Promise<void>}) {
  const {session}=useWorkspace();
  const [reason,setReason]=useState('');
  const form=useRef<HTMLFormElement>(null);
  const [observed,setObserved]=useState(value);
  useEffect(()=>{if(!observed && value) setObserved(value);},[observed,value]);
  const action=useBrowserAction<{paused:boolean;reason:string;revision:number},SchedulingReceipt>(`pull-scheduling-${number}`,
    (input,actionKey,signal)=>session.api.configurePullScheduling(number,input.paused,input.reason,input.revision,actionKey,signal),changed);
  useEffect(()=>{if(action.done){const details=form.current?.closest('details');if(details) details.open=false;}},[action.done]);
  const input=action.retained?.input;
  const paused=input?.paused ?? !observed?.paused;
  const submit=(event:FormEvent)=>{event.preventDefault();if(observed) void action.run({paused,reason:paused?reason:'',revision:observed.revision});};
  return <form ref={form} onSubmit={submit}><strong>{paused?'Pause new reviews':'Resume reviews'}</strong><p>Applies to Hosted and CLI. Issued reviews keep collecting.</p>
    {loading && !value && <small>Loading current settings…</small>}{Boolean(error) && <p role="alert">PR scheduling could not be loaded.</p>}
    {paused && <label>Reason (optional)<input value={input?.reason ?? reason} disabled={action.locked} maxLength={256} onChange={event=>setReason(event.target.value)} /></label>}
    <button type="submit" disabled={action.busy || !observed || new TextEncoder().encode(input?.reason ?? reason).byteLength>256}>{action.busy?'Saving…':action.locked?'Retry exact pause change':paused?'Pause reviews':'Resume reviews'}</button>
    {(Boolean(error) || Boolean(action.error) && !action.locked) && <button type="button" onClick={()=>void reload().then(updated=>{if(updated) setObserved(updated);})}>Reload current settings</button>}<ActionNotice error={action.error} done={action.done} />
  </form>;
}
