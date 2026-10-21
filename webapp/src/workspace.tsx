import { createContext, useContext, useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { ApiError } from './api';
import type { Api } from './api';
import type { BrowserSessionBinding } from './browser-session';
import type { Page, PresentationSnapshot, Project, Worker } from './types';

export type Session = { api: Api; project: Project; instanceId: string; nonce: string; controller: AbortController; projectController: AbortController; presentationSnapshot: PresentationSnapshot; worker: Worker | null; binding: BrowserSessionBinding };
export type WorkspaceValue = { session: Session; key: readonly string[]; home: string; selectWorker(worker: Worker): void; fail(error: unknown): void; ready: boolean };
export const WorkspaceContext = createContext<WorkspaceValue | null>(null);
export function useWorkspace(): WorkspaceValue { const value = useContext(WorkspaceContext); if (!value) throw new Error('Workspace unavailable'); return value; }

export function signalFor(session: Session, signal: AbortSignal): AbortSignal { return AbortSignal.any([session.controller.signal, signal]); }

export function errorText(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401 || error.status === 403 || error.code === 'unauthorized' || error.code === 'forbidden') return 'Access ended. Connect again with a credential that can read this project.';
    if (error.code === 'instance_mismatch') return 'The service identity changed. Connect again to verify this project.';
    if (error.code === 'invalid_binding') return 'Enter a valid project ID and project credential.';
    if (error.code === 'browser_storage_unavailable') return 'Enable session storage for this site to retain this tab’s connection across reloads.';
    if (error.status === 404) return 'This item is unavailable in the connected project.';
    if (error.code === 'connection_failed' || error.code === 'deadline_exceeded') return 'The service could not be reached in time. Try again when the connection is available.';
  }
  return 'This information could not be loaded. Try refreshing this view.';
}

export function usePaging() {
  const [state, setState] = useState({ trail: [''], index: 0 });
  return { cursor: state.trail[state.index], number: state.index + 1, previous: () => setState(value => ({ ...value, index: Math.max(0, value.index - 1) })), next: (cursor: string) => setState(value => ({ trail: [...value.trail.slice(0, value.index + 1), cursor], index: value.index + 1 })) };
}
export function Pager({ paging, page, label, busy }: { paging: ReturnType<typeof usePaging>; page?: Page<unknown>; label: string; busy: boolean }) {
  return <div className="pager"><span>Page {paging.number}{page ? ` · ${page.items.length} shown` : ''}{page && <small>{page.has_more ? 'More available' : 'End of list'}</small>}</span><div><button type="button" aria-label={`Previous ${label}`} disabled={paging.number === 1 || busy} onClick={paging.previous}>Previous</button><button type="button" aria-label={`Next ${label}`} disabled={!page?.has_more || !page.cursor || busy} onClick={() => { if (page?.cursor) paging.next(page.cursor); }}>Next</button></div></div>;
}
export function ReadState({ error, loading, empty, children }: { error: unknown; loading: boolean; empty: boolean; children: ReactNode }) {
  const { fail } = useWorkspace();
  useEffect(() => { if (error) fail(error); }, [error, fail]);
  if (error) return <div className="notice" role="alert">{errorText(error)}</div>;
  if (loading) return <p className="empty" role="status">Loading…</p>;
  if (empty) return <p className="empty">Nothing here yet.</p>;
  return children;
}
