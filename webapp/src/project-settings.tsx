import { useState } from 'react';
import type { FormEvent } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from './api';
import type { BrowserPresentationConfig, BrowserPresentationReceipt } from './api';
import { Pager, ReadState, signalFor, usePaging, useWorkspace } from './workspace';
import { ActionNotice, useBrowserAction } from './browser-actions';
import { validPresentationConfig } from './presentation';
import type { Presentation, ReviewGroup } from './types';

export type PresentationDraft = { name: string; brand_name: string; tagline: string; headings: string; theme: string; review_groups?: ReviewGroup[]; worker_order?: string[]; suggested_max_pr_files?: number };
function copyReviewGroups(groups: ReviewGroup[]): ReviewGroup[] {
  return groups.map(group => ({ ...group, pull_numbers: [...group.pull_numbers] }));
}
export function presentationDraft(value: BrowserPresentationConfig): PresentationDraft {
  return { name: value.name, brand_name: value.brand_name, tagline: value.tagline, headings: JSON.stringify(value.headings, null, 2), theme: JSON.stringify(value.theme, null, 2), ...(value.review_groups === undefined ? {} : { review_groups: copyReviewGroups(value.review_groups) }), ...(value.worker_order === undefined ? {} : { worker_order: [...value.worker_order] }), ...(value.suggested_max_pr_files===undefined ? {} : {suggested_max_pr_files:value.suggested_max_pr_files}) };
}
export function decodePresentationDraft(draft: PresentationDraft): BrowserPresentationConfig {
  if (draft.headings.length > 16 << 10 || draft.theme.length > 16 << 10) throw new ApiError('invalid_request', 'Appearance overrides exceed their supported size.');
  let headings: unknown, theme: unknown;
  try { headings = JSON.parse(draft.headings); theme = JSON.parse(draft.theme); } catch { throw new ApiError('invalid_request', 'Use JSON objects for heading and theme overrides.'); }
  const value = { name: draft.name, brand_name: draft.brand_name, tagline: draft.tagline, headings, theme, ...(Object.hasOwn(draft, 'review_groups') ? { review_groups: draft.review_groups } : {}), ...(Object.hasOwn(draft, 'worker_order') ? { worker_order: draft.worker_order } : {}), ...(Object.hasOwn(draft,'suggested_max_pr_files') ? {suggested_max_pr_files:draft.suggested_max_pr_files} : {}) };
  if (!validPresentationConfig(value)) throw new ApiError('invalid_request', 'Use supported headings, colors, contrast and bounded text.');
  return { ...value, ...(value.review_groups === undefined ? {} : { review_groups: copyReviewGroups(value.review_groups) }), ...(value.worker_order === undefined ? {} : { worker_order: [...value.worker_order] }), ...(value.suggested_max_pr_files===undefined ? {} : {suggested_max_pr_files:value.suggested_max_pr_files}) };
}
type PresentationAction = { worker: string; config: BrowserPresentationConfig; revision: number };

export function ProjectSettings() {
  const { session, key } = useWorkspace();
  const [reload, setReload] = useState(0);
  const query = useQuery({ queryKey: [...key, 'settings', 'presentation'], queryFn: ({ signal }) => session.api.presentation(signalFor(session, signal)), staleTime: Infinity, enabled: session.api.browserCapabilities.can_edit_presentation });
  if (!session.api.browserCapabilities.can_edit_presentation) return <section className="panel"><h2>Settings unavailable</h2><p>This connection does not permit project appearance changes.</p></section>;
  return <><div className="page-heading tab-heading"><h2>Settings</h2><p>Project appearance, worker order and review preferences.</p></div><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <PresentationEditor key={`${session.nonce}:${reload}`} initial={query.data.presentation} reload={async () => { const fresh = await query.refetch(); if (!fresh.error && !session.controller.signal.aborted) setReload(value => value + 1); }} />}</ReadState></>;
}

function WorkerOrderEditor({ value, change, disabled }: { value: string[]; change(value: string[]): void; disabled: boolean }) {
  const { session, key } = useWorkspace();
  const client = useQueryClient();
  const paging = usePaging(JSON.stringify(value));
  const query = useQuery({ queryKey: [...key, 'workers', 'ordering', paging.cursor], queryFn: ({ signal }) => session.api.workers({ limit: 100, ...(paging.cursor ? {cursor: paging.cursor} : {}) }, signalFor(session, signal)), staleTime: Infinity });
  const names = new Map(client.getQueriesData<{items: {id: string; name: string}[]}>({queryKey:[...key,'workers']}).flatMap(([,page]) => page?.items ?? []).map(worker => [worker.id,worker.name]));
  const move = (index: number, offset: number) => { const next = [...value]; [next[index],next[index+offset]] = [next[index+offset],next[index]]; change(next); };
  return <fieldset className="worker-order-editor" disabled={disabled}><legend>Worker order</legend><p className="field-note">Pinned workers appear first. Other workers follow alphabetically.</p>
    {value.length > 0 && <ol className="worker-order-list">{value.map((id,index) => <li key={id}><div><strong>{names.get(id) ?? id}</strong><span><button type="button" aria-label={`Move ${names.get(id) ?? id} up`} disabled={index===0} onClick={()=>move(index,-1)}>↑</button><button type="button" aria-label={`Move ${names.get(id) ?? id} down`} disabled={index===value.length-1} onClick={()=>move(index,1)}>↓</button><button type="button" aria-label={`Unpin ${names.get(id) ?? id}`} onClick={()=>change(value.filter(item=>item!==id))}>Unpin</button></span></div></li>)}</ol>}
    <label>Pin a worker<select aria-label="Pin a worker" value="" disabled={query.isPending || value.length>=100} onChange={event=>{if(event.target.value)change([...value,event.target.value]);}}><option value="">Choose a worker</option>{query.data?.items.filter(worker=>!value.includes(worker.id)).map(worker=><option key={worker.id} value={worker.id}>{worker.name}</option>)}</select></label>
    {query.error && <p className="notice">Worker choices could not be loaded.</p>}
    {(paging.number>1 || query.data?.has_more) && <Pager paging={paging} page={query.data} label="worker choices" busy={query.isFetching} />}
  </fieldset>;
}

function PresentationEditor({ initial, reload }: { initial: Presentation; reload(): Promise<void> }) {
  const { session, key } = useWorkspace();
  const queryClient = useQueryClient();
  const worker = session.worker?.id ?? '';
  const slot = JSON.stringify([worker, 'presentation']);
  const previous = session.actions.get<PresentationAction>(slot)?.input;
  const [draft, setDraft] = useState(() => presentationDraft(previous?.config ?? initial));
  const [revision, setRevision] = useState(previous?.revision ?? initial.revision);
  const [localError, setLocalError] = useState<unknown>();
  const [reloading, setReloading] = useState(false);
  const action = useBrowserAction<PresentationAction, BrowserPresentationReceipt>(slot,
    (input, actionKey, signal) => session.api.replacePresentation(input.worker, input.config, input.revision, actionKey, signal),
    async receipt => { setRevision(receipt.revision); await queryClient.invalidateQueries({ queryKey: [...key, 'settings', 'presentation'] }); });
  const change = <K extends Exclude<keyof PresentationDraft, 'review_groups'>>(field: K, value: PresentationDraft[K]) => { setDraft(current => ({ ...current, [field]: value })); setLocalError(undefined); action.clearNotice(); };
  const submit = (event: FormEvent) => {
    event.preventDefault(); setLocalError(undefined);
    try { void action.run({ worker, config: decodePresentationDraft(draft), revision }); } catch (error) { setLocalError(error); }
  };
  return <section className="panel"><form className="browser-action-form appearance-editor" onSubmit={submit}>
    <p className="field-note">Observed revision {revision}. Saving replaces the complete appearance record.</p>
    {!worker && <p className="notice">Choose a worker before saving appearance changes.</p>}
    <label>Project name<input value={draft.name} maxLength={200} required disabled={action.locked} onChange={event => change('name', event.target.value)} /></label>
    <label>Brand name<input value={draft.brand_name} maxLength={120} required disabled={action.locked} onChange={event => change('brand_name', event.target.value)} /></label>
    <label>Tagline<input value={draft.tagline} maxLength={120} required disabled={action.locked} onChange={event => change('tagline', event.target.value)} /></label>
    <label>Headings JSON<textarea value={draft.headings} rows={6} spellCheck={false} disabled={action.locked} onChange={event => change('headings', event.target.value)} /></label>
    <p className="field-note">An empty object keeps the default headings. Values render as literal text.</p>
    <label>Suggested maximum PR size<input aria-label="Suggested maximum PR size" type="number" min={1} max={100000} step={1} placeholder="Disabled" value={draft.suggested_max_pr_files || ''} disabled={action.locked} onChange={event=>change('suggested_max_pr_files',event.target.value===''?0:Number(event.target.value))} /></label>
    <p className="field-note">Optional warning in files. Leave blank to disable. This does not change the enforced maximum review size.</p>
    <WorkerOrderEditor value={draft.worker_order ?? []} change={value => change('worker_order', value)} disabled={action.locked} />
    <label>Theme JSON<textarea value={draft.theme} rows={6} spellCheck={false} disabled={action.locked} onChange={event => change('theme', event.target.value)} /></label>
    <p className="field-note">An empty object restores the default fire theme. Supported semantic colors and contrast are validated before saving.</p>
    <div className="action-buttons"><button className="primary" type="submit" disabled={!worker || action.busy || reloading}>{action.busy ? 'Saving…' : action.locked ? 'Retry exact appearance save' : 'Save appearance'}</button><button type="button" disabled={action.locked || reloading} onClick={() => { setReloading(true); void reload().finally(() => setReloading(false)); }}>{reloading ? 'Reloading…' : 'Reload current settings'}</button></div>
    <ActionNotice error={localError ?? action.error} done={action.done}>Appearance saved.</ActionNotice>
    {action.retained?.uncertain && <p className="field-note">Recovery key: <code>{action.retained.key}</code>. The displayed complete configuration, revision and selected worker are retained for exact retry. Keep this connection open until resolved.</p>}
  </form></section>;
}
