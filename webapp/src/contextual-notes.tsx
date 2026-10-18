import { Link } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { sessionIdentity } from './browser-session';
import { PageSkeleton } from './loading';
import { Pager, ReadState, signalFor, usePaging, useWorkspace, type Session } from './workspace';
import type { NoteOptions, NoteSummary } from './types';
import './coordination.css';

type NoteTarget = { workerId: string; jobId?: never } | { jobId: string; workerId?: never };
type NoteScope = { worker_id: string; include_assigned_jobs: false } | { job_id: string };
export function contextualNoteScope(target: NoteTarget): NoteScope | undefined {
  if (target.workerId !== undefined && target.jobId === undefined && sessionIdentity(target.workerId)) return { worker_id: target.workerId, include_assigned_jobs: false };
  if (target.jobId !== undefined && target.workerId === undefined && sessionIdentity(target.jobId)) return { job_id: target.jobId };
  return undefined;
}
export function contextualNotesQuery(session: Session, key: readonly string[], scope: NoteScope, cursor = '') {
  const options: NoteOptions = { ...scope, status: 'pending', limit: 5 };
  return { queryKey: [...key, 'notes', options, cursor], queryFn: ({ signal }: { signal: AbortSignal }) => session.api.notes({ ...options, ...(cursor ? { cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity };
}
function allNotesPath(home: string, scope: NoteScope): string {
  const params = new URLSearchParams({ status: 'all' });
  if ('worker_id' in scope) params.set('worker_id', scope.worker_id);
  else params.set('job_id', scope.job_id);
  return `${home}/notes?${params}`;
}
export function ContextualNoteRows({ notes, home }: { notes: NoteSummary[]; home: string }) {
  return <ul className="contextual-note-list">{notes.map(note => <li key={note.id}><span className="contextual-note-kind">{note.kind === 'source' ? 'Source' : 'Reminder'}</span><p className="contextual-note-preview">{note.body_preview || 'No preview available.'}</p><Link to={`${home}/notes/${encodeURIComponent(note.id)}`}>Read note</Link></li>)}</ul>;
}
export function ContextualNotes(target: NoteTarget) {
  const { key } = useWorkspace();
  const scope = contextualNoteScope(target);
  if (!scope) return <p className="notice" role="alert">Choose one worker or job to view its pending notes.</p>;
  // Target/connection changes remount pagination rather than reusing a cursor.
  return <PendingNotes key={JSON.stringify([key, scope])} scope={scope} />;
}
function PendingNotes({ scope }: { scope: NoteScope }) {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const query = useQuery(contextualNotesQuery(session, key, scope, paging.cursor));
  const page = query.data;
  if (!query.isPending && !query.error && page && page.items.length === 0 && !page.has_more && paging.number === 1) return <p className="contextual-notes-history"><Link to={allNotesPath(home, scope)}>Notes history</Link></p>;
  return <section className="panel contextual-notes" aria-label="Pending notes"><div className="contextual-notes-heading"><h3>Pending notes</h3><Link to={allNotesPath(home, scope)}>View all notes and history</Link></div><ReadState error={query.error} loading={false} empty={false}>{query.isPending ? <PageSkeleton rows={1} label="Loading pending notes" /> : page?.items.length ? <ContextualNoteRows notes={page.items} home={home} /> : <p className="contextual-notes-empty">No pending notes on this page.</p>}</ReadState>{(page && (page.items.length > 0 || page.has_more || paging.number > 1)) && <Pager paging={paging} page={page} label="pending notes" busy={query.isFetching} />}</section>;
}
