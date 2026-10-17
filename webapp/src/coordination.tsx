import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useParams, useSearchParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { Pager, ReadState, signalFor, usePaging, useWorkspace } from './workspace';
import type { Note, NoteHistorySummary, NoteOptions, NoteRevision, NoteSummary, WorkerPause, WorkerPrimary } from './types';
import { useProjectHeading } from './presentation-context';
import './coordination.css';

const pageSize = 20;
const date = (value: string) => new Date(value).toLocaleString();
const workerPath = (home: string, id: string) => `${home}/workers/${encodeURIComponent(id)}`;
const jobPath = (home: string, id: string) => `${home}/jobs/${encodeURIComponent(id)}`;
const notePath = (home: string, id: string) => `${home}/notes/${encodeURIComponent(id)}`;
export function notesPath(home: string, filters: { worker_id?: string; job_id?: string } = {}) {
  const params = new URLSearchParams();
  if (filters.worker_id) params.set('worker_id', filters.worker_id);
  if (filters.job_id) params.set('job_id', filters.job_id);
  return `${home}/notes${params.size ? `?${params}` : ''}`;
}

export function PauseContent({ value }: { value: WorkerPause }) {
  return <><h3>Explicit pause</h3><p><strong>{value.paused ? 'Paused' : 'Not paused'}</strong> · Pause revision {value.revision}</p><p className="full-text">{value.reason || 'No pause reason recorded.'}</p><p className="item-meta">A coordination choice, not agent presence or an execution control.</p></>;
}
export function PrimaryContent({ value, home }: { value: WorkerPrimary; home: string }) {
  return <><h3>Primary job</h3>{value.job_id ? <p><Link to={jobPath(home, value.job_id)}>Open primary job {value.job_id}</Link></p> : <p>No primary job selected.</p>}<p className="item-meta">Focus revision {value.revision}. Focus is independent of job status and other assignments.</p></>;
}
export function WorkerCoordinationView() {
  const { workerId = '' } = useParams();
  return <WorkerDetails key={workerId} workerId={workerId} />;
}
function WorkerDetails({ workerId }: { workerId: string }) {
  const { session, key, home } = useWorkspace();
  const [pauseHistory, setPauseHistory] = useState(false);
  const [primaryHistory, setPrimaryHistory] = useState(false);
  const worker = useQuery({ queryKey: [...key, 'worker', workerId], queryFn: ({ signal }) => session.api.worker(workerId, signalFor(session, signal)), staleTime: Infinity });
  const pause = useQuery({ queryKey: [...key, 'worker-pause', workerId], queryFn: ({ signal }) => session.api.workerPause(workerId, signalFor(session, signal)), staleTime: Infinity });
  const primary = useQuery({ queryKey: [...key, 'worker-primary', workerId], queryFn: ({ signal }) => session.api.workerPrimary(workerId, signalFor(session, signal)), staleTime: Infinity });
  return <div className="coordination"><section className="panel"><ReadState error={worker.error} loading={worker.isPending} empty={false}>{worker.data && <><p className="eyebrow">Worker details</p><h2>{worker.data.name}</h2><p className="item-meta">{worker.data.id}</p><Link to={notesPath(home, { worker_id: workerId })}>Notes for this worker</Link></>}</ReadState></section>
    <section className="panel"><ReadState error={pause.error} loading={pause.isPending} empty={false}>{pause.data && <PauseContent value={pause.data} />}</ReadState><button type="button" aria-expanded={pauseHistory} onClick={() => setPauseHistory(value => !value)}>{pauseHistory ? 'Hide pause history' : 'Show pause history'}</button>{pauseHistory && <PauseHistory workerId={workerId} />}</section>
    <section className="panel"><ReadState error={primary.error} loading={primary.isPending} empty={false}>{primary.data && <PrimaryContent value={primary.data} home={home} />}</ReadState><button type="button" aria-expanded={primaryHistory} onClick={() => setPrimaryHistory(value => !value)}>{primaryHistory ? 'Hide focus history' : 'Show focus history'}</button>{primaryHistory && <PrimaryHistory workerId={workerId} />}</section>
  </div>;
}
function PauseHistory({ workerId }: { workerId: string }) {
  const { session, key } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'worker-pause-history', workerId, paging.cursor], queryFn: ({ signal }) => session.api.workerPauseHistory(workerId, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  return <div className="coordination-history"><h4>Pause history</h4><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><ol>{query.data?.items.map(row => <li key={row.revision}><strong>Revision {row.revision} · {row.paused ? 'Paused' : 'Not paused'}</strong><p className="full-text">{row.reason || 'No reason recorded.'}</p><p className="item-meta">Recorded by {row.actor_id} · Worker {row.actor_worker_id || 'not bound'} · <time dateTime={row.created_at}>{date(row.created_at)}</time></p></li>)}</ol></ReadState><Pager paging={paging} page={query.data} label="pause history" busy={query.isFetching} /></div>;
}
function PrimaryHistory({ workerId }: { workerId: string }) {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'worker-primary-history', workerId, paging.cursor], queryFn: ({ signal }) => session.api.workerPrimaryHistory(workerId, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  return <div className="coordination-history"><h4>Focus history</h4><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><ol>{query.data?.items.map(row => <li key={row.revision}><strong>Revision {row.revision}</strong><p>{row.job_id ? <Link to={jobPath(home, row.job_id)}>Job {row.job_id}</Link> : 'No primary job selected.'}</p><p className="full-text">{row.reason}</p><p className="item-meta">Recorded by {row.actor_id} · Worker {row.actor_worker_id || 'not bound'} · <time dateTime={row.created_at}>{date(row.created_at)}</time></p></li>)}</ol></ReadState><Pager paging={paging} page={query.data} label="focus history" busy={query.isFetching} /></div>;
}

type FilterValues = { worker_id: string; job_id: string; phase: string; kind: string; status: string; include_assigned_jobs: boolean };
export function NotesView() {
  const [params, setParams] = useSearchParams();
  return <NotesBrowser key={params.toString()} params={params} change={setParams} />;
}
function NotesBrowser({ params, change }: { params: URLSearchParams; change: (value: URLSearchParams) => void }) {
  const label = useProjectHeading('notes');
  const [values, setValues] = useState<FilterValues>({ worker_id: params.get('worker_id') ?? '', job_id: params.get('job_id') ?? '', phase: params.get('phase') ?? '', kind: params.get('kind') ?? '', status: params.get('status') ?? 'pending', include_assigned_jobs: params.get('include_assigned_jobs') === 'true' });
  const [formError, setFormError] = useState('');
  const options: NoteOptions = {};
  for (const field of ['worker_id', 'job_id', 'phase', 'kind', 'status'] as const) {
    if (params.has(field)) Object.assign(options, { [field]: params.get(field) });
  }
  if (params.has('include_assigned_jobs')) Object.assign(options, { include_assigned_jobs: params.get('include_assigned_jobs') === 'true' });
  const malformed = ['worker_id', 'job_id', 'phase', 'kind', 'status', 'include_assigned_jobs'].some(field => params.getAll(field).length > 1)
    || params.has('include_assigned_jobs') && !['true', 'false'].includes(params.get('include_assigned_jobs') ?? '');
  const update = (field: keyof FilterValues, value: string | boolean) => setValues(current => ({ ...current, [field]: value }));
  function apply(event: FormEvent) {
    event.preventDefault();
    if (values.include_assigned_jobs && !values.worker_id) { setFormError('Choose a worker before including assigned jobs.'); return; }
    setFormError('');
    const next = new URLSearchParams();
    for (const field of ['worker_id', 'job_id', 'phase', 'kind', 'status'] as const) if (values[field]) next.set(field, values[field]);
    if (values.include_assigned_jobs) next.set('include_assigned_jobs', 'true');
    change(next);
  }
  return <div className="coordination"><div className="page-heading"><p className="eyebrow">Project coordination</p><h2>{label}</h2><p>Project reminders and source notes. Reading leaves their status unchanged.</p></div>
    <form className="coordination-filters" onSubmit={apply} aria-label="Note filters">
      <label>Worker ID<input value={values.worker_id} onChange={event => update('worker_id', event.target.value)} /></label>
      <label>Job ID<input value={values.job_id} onChange={event => update('job_id', event.target.value)} /></label>
      <label>Phase<input value={values.phase} onChange={event => update('phase', event.target.value)} /></label>
      <label>Kind<select value={values.kind} onChange={event => update('kind', event.target.value)}><option value="">All kinds</option><option value="reminder">Reminder</option><option value="source">Source</option></select></label>
      <label>Status<select value={values.status} onChange={event => update('status', event.target.value)}><option value="pending">Pending</option><option value="consumed">Consumed</option><option value="dismissed">Dismissed</option><option value="all">All statuses</option></select></label>
      <label className="checkbox-label"><input type="checkbox" checked={values.include_assigned_jobs} onChange={event => update('include_assigned_jobs', event.target.checked)} />Include this worker’s assigned jobs</label>
      <button type="submit">Apply note filters</button>
      {formError && <p className="notice" role="alert">{formError}</p>}
    </form>
    {malformed ? <p className="notice" role="alert">The note filters are invalid. Apply valid filters to continue.</p> : <NotesList options={options} />}
  </div>;
}
function NotesList({ options }: { options: NoteOptions }) {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'notes', options, paging.cursor], queryFn: ({ signal }) => session.api.notes({ ...options, limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><NoteSummaryRows rows={query.data?.items ?? []} home={home} /></ReadState><Pager paging={paging} page={query.data} label="notes" busy={query.isFetching} /></section>;
}
function NoteScope({ value, home }: { value: Pick<NoteSummary, 'worker_id' | 'job_id' | 'phase'>; home: string }) {
  return <dl className="coordination-scope"><div><dt>Worker selector</dt><dd>{value.worker_id ? <Link to={workerPath(home, value.worker_id)}>{value.worker_id}</Link> : 'No worker selector'}</dd></div><div><dt>Job selector</dt><dd>{value.job_id ? <Link to={jobPath(home, value.job_id)}>{value.job_id}</Link> : 'No job selector'}</dd></div><div><dt>Phase selector</dt><dd className="full-text">{value.phase || 'No phase selector'}</dd></div></dl>;
}
export function NoteSummaryRows({ rows, home }: { rows: NoteSummary[]; home: string }) {
  return <ul className="coordination-notes">{rows.map(note => <li key={note.id}><p><span className="badge neutral">{note.kind} · {note.status}</span> · Revision {note.revision}</p><NoteScope value={note} home={home} /><p className="full-text">{note.body_preview}</p><p className="item-meta">{note.body_truncated ? 'Truncated preview.' : 'Preview.'} Open the note to read its full body.</p><Link to={notePath(home, note.id)}>Read note {note.id}</Link></li>)}</ul>;
}
export function NoteContent({ value, home }: { value: Note; home: string }) {
  return <section className="panel brief"><h2>Note</h2><p><span className="badge neutral">{value.kind} · {value.status}</span> · Revision {value.revision}</p><NoteScope value={value} home={home} /><h3>Full body</h3><p className="full-text">{value.body}</p><h3>{value.status === 'dismissed' ? 'Dismissal reason' : 'Status reason'}</h3><p className="full-text">{value.status_reason || 'No status reason recorded.'}</p><dl className="metadata"><div><dt>Note ID</dt><dd>{value.id}</dd></div><div><dt>Created</dt><dd><time dateTime={value.created_at}>{date(value.created_at)}</time></dd></div><div><dt>Updated</dt><dd><time dateTime={value.updated_at}>{date(value.updated_at)}</time></dd></div></dl></section>;
}
export function NoteView() {
  const { noteId = '' } = useParams();
  return <CurrentNote key={noteId} noteId={noteId} />;
}
function CurrentNote({ noteId }: { noteId: string }) {
  const { session, key, home } = useWorkspace();
  const [history, setHistory] = useState(false);
  const query = useQuery({ queryKey: [...key, 'note', noteId], queryFn: ({ signal }) => session.api.note(noteId, signalFor(session, signal)), staleTime: Infinity });
  return <div className="coordination"><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <NoteContent value={query.data} home={home} />}</ReadState><section className="panel"><button type="button" aria-expanded={history} onClick={() => setHistory(value => !value)}>{history ? 'Hide note history' : 'Show note history'}</button>{history && <NoteHistory noteId={noteId} />}</section></div>;
}
export function NoteHistoryRows({ rows, home, noteId }: { rows: NoteHistorySummary[]; home: string; noteId: string }) {
  return <ol className="coordination-notes">{rows.map(row => <li key={row.revision}><p><Link to={`${notePath(home, noteId)}/history/${row.revision}`}>Read revision {row.revision}</Link> · {row.kind} · {row.status}</p><NoteScope value={row} home={home} /><p className="full-text">{row.body_preview}</p><p className="item-meta">{row.body_truncated ? 'Truncated preview.' : 'Preview.'} Open the captured revision to read its full body.</p><p className="item-meta">Recorded by {row.actor_id} · Worker {row.actor_worker_id || 'not bound'} · <time dateTime={row.recorded_at}>{date(row.recorded_at)}</time></p></li>)}</ol>;
}
function NoteHistory({ noteId }: { noteId: string }) {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'note-history', noteId, paging.cursor], queryFn: ({ signal }) => session.api.noteHistory(noteId, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  return <div className="coordination-history"><h3>Note history</h3><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><NoteHistoryRows rows={query.data?.items ?? []} home={home} noteId={noteId} /></ReadState><Pager paging={paging} page={query.data} label="note history" busy={query.isFetching} /></div>;
}
export function NoteRevisionContent({ value, home }: { value: NoteRevision; home: string }) {
  return <><section className="panel"><h2>Captured note revision {value.revision}</h2><p>This immutable snapshot does not describe current state.</p><p className="item-meta">Recorded by {value.actor_id} · Worker {value.actor_worker_id || 'not bound'} · <time dateTime={value.recorded_at}>{date(value.recorded_at)}</time></p><Link to={notePath(home, value.id)}>Read current note</Link></section><NoteContent value={value} home={home} /></>;
}
export function NoteRevisionView() {
  const { noteId = '', revision = '' } = useParams();
  const { session, key, home } = useWorkspace();
  const number = /^\d+$/u.test(revision) ? Number(revision) : NaN;
  const query = useQuery({ queryKey: [...key, 'note-revision', noteId, revision], queryFn: ({ signal }) => session.api.noteRevision(noteId, number, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  return <div className="coordination"><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <NoteRevisionContent value={query.data} home={home} />}</ReadState></div>;
}
