import { useState } from 'react';
import type { FormEvent } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from './api';
import type { BrowserPresentationConfig, BrowserPresentationReceipt } from './api';
import { ReadState, signalFor, useWorkspace } from './workspace';
import { ActionNotice, useBrowserAction } from './browser-actions';
import { validPresentationConfig } from './presentation';
import type { Presentation } from './types';

export type PresentationDraft = { name: string; brand_name: string; tagline: string; headings: string; theme: string };
export function presentationDraft(value: BrowserPresentationConfig): PresentationDraft {
  return { name: value.name, brand_name: value.brand_name, tagline: value.tagline, headings: JSON.stringify(value.headings, null, 2), theme: JSON.stringify(value.theme, null, 2) };
}
export function decodePresentationDraft(draft: PresentationDraft): BrowserPresentationConfig {
  if (draft.headings.length > 16 << 10 || draft.theme.length > 16 << 10) throw new ApiError('invalid_request', 'Appearance overrides exceed their supported size.');
  let headings: unknown, theme: unknown;
  try { headings = JSON.parse(draft.headings); theme = JSON.parse(draft.theme); } catch { throw new ApiError('invalid_request', 'Use JSON objects for heading and theme overrides.'); }
  const value = { name: draft.name, brand_name: draft.brand_name, tagline: draft.tagline, headings, theme };
  if (!validPresentationConfig(value)) throw new ApiError('invalid_request', 'Use supported headings, colors, contrast and bounded text.');
  return value;
}
type PresentationAction = { worker: string; config: BrowserPresentationConfig; revision: number };

export function ProjectSettings() {
  const { session, key } = useWorkspace();
  const [reload, setReload] = useState(0);
  const query = useQuery({ queryKey: [...key, 'settings', 'presentation'], queryFn: ({ signal }) => session.api.presentation(signalFor(session, signal)), staleTime: Infinity, enabled: session.api.browserCapabilities.can_edit_presentation });
  if (!session.api.browserCapabilities.can_edit_presentation) return <section className="panel"><h2>Settings unavailable</h2><p>This connection does not permit project appearance changes.</p></section>;
  return <><div className="page-heading"><p className="eyebrow">Project settings</p><h2>Appearance</h2><p>Complete private-workspace presentation. Public website approvals are separate.</p></div><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <PresentationEditor key={`${session.nonce}:${reload}`} initial={query.data.presentation} reload={async () => { const fresh = await query.refetch(); if (!fresh.error && !session.controller.signal.aborted) setReload(value => value + 1); }} />}</ReadState></>;
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
  const change = (field: keyof PresentationDraft, value: string) => { setDraft(current => ({ ...current, [field]: value })); setLocalError(undefined); action.clearNotice(); };
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
    <label>Theme JSON<textarea value={draft.theme} rows={6} spellCheck={false} disabled={action.locked} onChange={event => change('theme', event.target.value)} /></label>
    <p className="field-note">An empty object restores the default fire theme. Supported semantic colors and contrast are validated before saving.</p>
    <div className="action-buttons"><button className="primary" type="submit" disabled={!worker || action.busy || reloading}>{action.busy ? 'Saving…' : action.locked ? 'Retry exact appearance save' : 'Save appearance'}</button><button type="button" disabled={action.locked || reloading} onClick={() => { setReloading(true); void reload().finally(() => setReloading(false)); }}>{reloading ? 'Reloading…' : 'Reload current settings'}</button></div>
    <ActionNotice error={localError ?? action.error} done={action.done}>Appearance saved.</ActionNotice>
    {action.retained?.uncertain && <p className="field-note">Recovery key: <code>{action.retained.key}</code>. The displayed complete configuration, revision and selected worker are retained for exact retry. Keep this connection open until resolved.</p>}
  </form></section>;
}
