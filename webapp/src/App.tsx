import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { Link, NavLink, Route, Routes, useLocation, useMatch, useNavigate, useParams, useSearchParams } from 'react-router';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Api, ApiError } from './api';
import { refreshEventQueries } from './queries';
import { clearBrowserSession, loadBrowserSession, saveBrowserSession, terminalBrowserSession, type BrowserSessionBinding } from './browser-session';
import flameEmber from './flame-ember.svg?no-inline';
import type { Checkpoint, InboxItem, JobSummary, LiveStatus, Page, Project, Worker, ReviewAttemptSummary, ReviewFinding, ReviewPullSummary, ReviewRunSummary, ReviewSnapshot } from './types';

const pageSize = 20;
type Session = { api: Api; project: Project; instanceId: string; nonce: string; controller: AbortController; worker: Worker | null; binding: BrowserSessionBinding };
type WorkspaceValue = { session: Session; key: readonly string[]; home: string; selectWorker(worker: Worker): void; fail(error: unknown): void; ready: boolean };
const WorkspaceContext = createContext<WorkspaceValue | null>(null);
function useWorkspace(): WorkspaceValue { const value = useContext(WorkspaceContext); if (!value) throw new Error('Workspace unavailable'); return value; }
function nonce(): string { return Array.from(crypto.getRandomValues(new Uint32Array(4)), value => value.toString(16).padStart(8, '0')).join(''); }
function signalFor(session: Session, signal: AbortSignal): AbortSignal { return AbortSignal.any([session.controller.signal, signal]); }
function terminal(error: unknown): boolean { return error instanceof ApiError && terminalBrowserSession(error); }
function errorText(error: unknown): string {
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
function time(value: string): string { return new Date(value).toLocaleString(); }

export default function App() {
  const navigate = useNavigate();
  const location = useLocation();
  const route = useRef(location);
  route.current = location;
  const match = useMatch('/projects/:projectId/*');
  const conversationMatch = useMatch('/projects/:projectId/inbox/:conversationId');
  const routeProject = match?.params.projectId;
  const queryClient = useQueryClient();
  const [session, setSession] = useState<Session | null>(null);
  const current = useRef<Session | null>(null);
  const pending = useRef<{ api: Api; controller: AbortController; projectId: string; routeKey: string } | null>(null);
  const saved = useRef<BrowserSessionBinding | null>(null);
  const [resumable, setResumable] = useState<BrowserSessionBinding | null>(null);
  const [reconnect, setReconnect] = useState(0);
  const [notice, setNotice] = useState('');
  const [connecting, setConnecting] = useState(false);
  const clear = useCallback((message = '') => {
    const expiring = pending.current?.api ?? current.current?.api;
    if (expiring) void expiring.endBrowserSession().catch(() => {});
    else if (saved.current) expireBinding(saved.current);
    clearBrowserSession(); saved.current = null; setResumable(null);
    pending.current?.controller.abort(); pending.current?.api.close(); pending.current = null;
    const old = current.current;
    current.current = null;
    old?.controller.abort(); old?.api.close();
    if (old) { void queryClient.cancelQueries({ queryKey: ['fc', old.nonce] }); queryClient.removeQueries({ queryKey: ['fc', old.nonce] }); }
    setSession(null); setConnecting(false); setNotice(message);
  }, [queryClient]);
  useEffect(() => {
    if (current.current && routeProject !== current.current.project.id) clear('Connect to open this project.');
    if (pending.current && location.key !== pending.current.routeKey) clear('Connect to open this project.');
  }, [routeProject, location.key, clear]);
  useEffect(() => () => {
    pending.current?.controller.abort(); pending.current?.api.close(); pending.current = null;
    const old = current.current; old?.controller.abort(); old?.api.close();
    current.current = null;
    if (old) queryClient.removeQueries({ queryKey: ['fc', old.nonce] });
  }, [queryClient]);
  useEffect(() => {
    if (current.current || pending.current) return;
    const binding = loadBrowserSession(routeProject, expireBinding);
    saved.current = binding; setResumable(binding);
    if (binding) void connect(binding.projectID, undefined, binding);
  }, [routeProject, reconnect]);
  async function connect(projectId: string, token?: string, restored?: BrowserSessionBinding) {
    if (!restored) clear();
    let api: Api;
    try { api = restored ? Api.resume(restored.projectID, restored.sessionID, restored.instanceID) : new Api(token ?? '', projectId); } catch (error) { setNotice(errorText(error)); return; }
    const controller = new AbortController();
    const attempt = { api, controller, projectId, routeKey: location.key };
    pending.current = attempt; setConnecting(true); setNotice('');
    const active = () => pending.current === attempt && !controller.signal.aborted && route.current.key === attempt.routeKey;
    try {
      const project = await api.connect(controller.signal);
      if (!active()) return;
      if (!api.instanceId) throw new ApiError('invalid_response', 'The service identity is unavailable.');
      let binding = restored;
      if (!binding) {
        const started = await api.startBrowserSession(controller.signal);
        if (!active()) return;
        binding = { version: 1, projectID: started.project_id, instanceID: started.instance_id, sessionID: started.session_id, expiresAt: started.expires_at, selectedworkerID: '' };
      }
      const worker = binding.selectedworkerID ? await restoredWorker(api, binding.selectedworkerID, controller.signal) : null;
      if (!active()) return;
      binding = { ...binding, selectedworkerID: worker?.id ?? '' };
      if (!saveBrowserSession(binding)) throw new ApiError('browser_storage_unavailable', 'The browser session could not be retained safely.');
      saved.current = binding; setResumable(binding);
      const next: Session = { api, project, instanceId: api.instanceId!, nonce: nonce(), controller, worker, binding };
      pending.current = null; current.current = next; setSession(next); setConnecting(false);
      if (routeProject !== project.id) navigate(`/projects/${encodeURIComponent(project.id)}`);
    } catch (error) {
      if (!restored) void api.endBrowserSession().catch(() => {});
      api.close();
      if (pending.current === attempt) {
        if (terminal(error)) clear(errorText(error));
        else { pending.current = null; setConnecting(false); setNotice(errorText(error)); }
      }
    }
  }
  function selectWorker(worker: Worker) {
    const old = current.current;
    if (!old) return;
    if (old.worker?.id !== worker.id) {
      old.controller.abort();
      void queryClient.cancelQueries({ queryKey: ['fc', old.nonce] });
      queryClient.removeQueries({ queryKey: ['fc', old.nonce] });
      const binding = { ...old.binding, selectedworkerID: worker.id };
      if (!saveBrowserSession(binding)) {
        clear('The selected worker could not be retained. Enable session storage and connect again.');
        return;
      }
      saved.current = binding; setResumable(binding);
      const next = { ...old, worker, binding, nonce: nonce(), controller: new AbortController() };
      current.current = next; setSession(next);
    }
    if (old.worker !== null || conversationMatch?.params.projectId !== old.project.id) {
      navigate(`/projects/${encodeURIComponent(old.project.id)}/inbox`);
    }
  }
  const connected = session && routeProject === session.project.id;
  return <>
    <header className="mast"><div className="mast-inner"><Link className="brand" to={connected ? `/projects/${encodeURIComponent(session.project.id)}` : '/'}><Flame />FireController</Link><span className="mast-label">Project workspace</span></div></header>
    {connected ? <Workspace key={session.nonce} session={session} selectWorker={selectWorker} disconnect={() => { clear(); navigate('/'); }} fail={error => { if (terminal(error)) clear(errorText(error)); }} />
      : <Connect key={routeProject ?? 'root'} projectId={routeProject ?? ''} busy={connecting} notice={notice} onConnect={connect} reconnect={resumable ? () => setReconnect(value => value + 1) : undefined} cancel={() => clear('Connection cancelled.')} />}
    <footer className="footer"><span className="attribution">Powered by <a href="https://github.com/benhook1013/FireController" target="_blank" rel="noreferrer noopener">FireController</a></span><span>Read-only project workspace</span></footer>
  </>;
}

function expireBinding(binding: BrowserSessionBinding): void {
  const api = Api.resume(binding.projectID, binding.sessionID, binding.instanceID);
  void api.endBrowserSession().catch(() => {}).finally(() => api.close());
}
async function restoredWorker(api: Api, workerID: string, signal: AbortSignal): Promise<Worker | null> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 5000);
  const combined = AbortSignal.any([signal, controller.signal]);
  const cursors = new Set<string>();
  let cursor: string | undefined;
  try {
    do {
      if (combined.aborted) throw new ApiError('deadline_exceeded', 'The selected worker could not be verified in time.');
      const page = await api.workers({ limit: 100, ...(cursor ? { cursor } : {}) }, combined);
      const found = page.items.find(worker => worker.id === workerID);
      if (found) return found;
      if (!page.has_more) return null;
      if (!page.cursor || cursors.has(page.cursor)) throw new ApiError('invalid_response', 'The worker pages did not advance.');
      cursor = page.cursor; cursors.add(cursor);
    } while (!combined.aborted);
    throw new ApiError('deadline_exceeded', 'The selected worker could not be verified in time.');
  } finally { clearTimeout(timer); }
}

function Flame() { return <img className="flame" src={flameEmber} alt="" aria-hidden="true" />; }
function Connect({ projectId, busy, notice, onConnect, reconnect, cancel }: { projectId: string; busy: boolean; notice: string; onConnect(project: string, token: string): void; reconnect?: () => void; cancel(): void }) {
  const [project, setProject] = useState(projectId);
  const [token, setToken] = useState('');
  function submit(event: FormEvent) { event.preventDefault(); const supplied = token; setToken(''); onConnect(project, supplied); }
  return <main className="connect-layout"><section className="connect-intro"><p className="eyebrow">Your project, in view</p><h1>Keep work<br /> moving.</h1><p>A clear view of your workers, jobs and conversations.</p><div className="intro-rule" /><p className="quiet-light">Connect to a project on this service to begin.</p></section>
    <section className="connect-card"><p className="eyebrow">FireController</p><h2>Connect to a project</h2><p className="muted">Use the project ID and a credential with read access.</p>
      {notice && <div className="notice" role="alert">{notice}</div>}
      {reconnect && !busy && <button type="button" onClick={reconnect}>Reconnect to project</button>}
      <form onSubmit={submit} autoComplete="off"><label htmlFor="project-id">Project ID</label><input id="project-id" name="project-id" value={project} onChange={event => setProject(event.target.value)} required maxLength={128} spellCheck={false} autoCapitalize="none" disabled={busy} />
        <label htmlFor="project-credential">Project credential</label><input id="project-credential" name="project-credential" type="password" value={token} onChange={event => setToken(event.target.value)} required maxLength={4096} autoComplete="off" disabled={busy} />
        <p className="field-note">Exchanged for a read-only session lasting up to 24 hours in this tab, including reloads. Disconnect to clear access and project data.</p>
        <button className="primary" type="submit" disabled={busy}>{busy ? 'Connecting…' : 'Connect to project'}</button>{busy && <button type="button" onClick={cancel}>Cancel connection</button>}
      </form></section></main>;
}

function Workspace({ session, selectWorker, disconnect, fail }: { session: Session; selectWorker(worker: Worker): void; disconnect(): void; fail(error: unknown): void }) {
  const queryClient = useQueryClient();
  const key = useMemo(() => ['fc', session.nonce, session.instanceId, session.project.id, session.worker?.id ?? ''] as const, [session]);
  const home = `/projects/${encodeURIComponent(session.project.id)}`;
  const [live, setLive] = useState<LiveStatus>('connecting');
  const [ready, setReady] = useState(false);
  const [feedError, setFeedError] = useState<unknown>(null);
  const [attempt, setAttempt] = useState(0);
  const [refreshing, setRefreshing] = useState(false);
  const failRef = useRef(fail);
  useEffect(() => { failRef.current = fail; }, [fail]);
  const onFailure = useCallback((error: unknown) => failRef.current(error), []);
  useEffect(() => {
    if (!session.worker) return;
    const controller = new AbortController();
    const signal = signalFor(session, controller.signal);
    let active = true;
    setLive('connecting'); setFeedError(null);
    void session.api.watchMessages(session.worker.id, { limit: pageSize }, {
      snapshot: async value => {
        if (!active || signal.aborted) return;
        queryClient.setQueryData([...key, 'inbox', ''], value.inbox);
        setReady(true);
        await refreshEventQueries(queryClient, [...key, 'inbox'], signal, true);
        await refreshEventQueries(queryClient, [...key, 'thread'], signal);
      },
      events: async value => {
        if (!active || signal.aborted || value.items.length === 0) return;
        await refreshEventQueries(queryClient, [...key, 'inbox'], signal);
        for (const conversation of new Set(value.items.map(item => item.payload.conversation_id))) {
          if (!active || signal.aborted) return;
          await refreshEventQueries(queryClient, [...key, 'thread', conversation], signal);
        }
      },
      status: value => { if (active && !signal.aborted) setLive(value); },
    }, signal).catch(error => {
      if (!active || signal.aborted) return;
      setFeedError(error); setLive('closed'); onFailure(error);
    });
    return () => { active = false; controller.abort(); };
  }, [session, key, queryClient, onFailure, attempt]);
  async function refresh() {
    setRefreshing(true);
    try { await queryClient.invalidateQueries({ queryKey: key, refetchType: 'active' }, { throwOnError: true }); }
    catch (error) { onFailure(error); }
    finally { if (!session.controller.signal.aborted) setRefreshing(false); }
  }
  const value = useMemo(() => ({ session, key, home, selectWorker, fail: onFailure, ready }), [session, key, home, selectWorker, onFailure, ready]);
  const liveLabel = !session.worker ? 'Choose a worker' : live === 'live' ? 'Live inbox connected' : live === 'reconnecting' ? 'Reconnecting inbox…' : live === 'closed' ? 'Inbox disconnected' : 'Connecting inbox…';
  return <WorkspaceContext.Provider value={value}><main className="workspace">
    <section className="hero"><div className="hero-project"><p className="eyebrow">Project workspace <span className="read-only">Read only</span></p><h1>{session.project.name}</h1><p className="repository">{session.project.repository}</p><p className="identity">Project {session.project.id}</p></div>
      <div className="hero-side"><div className="connection-card"><span className="eyebrow">Connected project</span><strong>Work, with context.</strong><p>Browse jobs and read worker conversations.</p><div className="hero-actions"><button type="button" onClick={() => void refresh()} disabled={refreshing}>{refreshing ? 'Refreshing…' : 'Refresh visible data'}</button><button className="quiet-button" type="button" onClick={disconnect}>Disconnect</button></div></div></div></section>
    <nav className="navigation" aria-label="Project navigation"><NavLink to={home} end>Jobs</NavLink><NavLink to={`${home}/workers`}>Workers</NavLink><NavLink to={`${home}/inbox`}>Inbox</NavLink><NavLink to={`${home}/reviews`}>Reviews</NavLink><span className="connection-note">Connected</span></nav>
    <div className="workspace-grid"><aside className="sidebar"><WorkerList compact /><section className="feed-card" aria-label="Inbox connection"><span className={`status-dot ${live === 'live' && session.worker ? 'online' : ''}`} /><strong role="status">{liveLabel}</strong><p>{session.worker ? session.worker.name : 'Select a worker to follow incoming messages.'}</p>{Boolean(feedError) && <><p className="error-copy">{errorText(feedError)}</p><button type="button" onClick={() => { setReady(false); setAttempt(value => value + 1); }}>Reconnect inbox</button></>}</section></aside>
      <div className="main-content"><Routes><Route path="/projects/:projectId" element={<Jobs />} /><Route path="/projects/:projectId/workers" element={<Workers />} /><Route path="/projects/:projectId/jobs/:jobId" element={<JobDetail />} /><Route path="/projects/:projectId/inbox" element={<Inbox />} /><Route path="/projects/:projectId/inbox/:conversationId" element={<Conversation />} /><Route path="/projects/:projectId/reviews" element={<ReviewQueueView />} /><Route path="/projects/:projectId/reviews/pulls/:number" element={<ReviewPullView />} /><Route path="/projects/:projectId/reviews/runs/:runId" element={<ReviewRunView />} /><Route path="*" element={<section className="panel"><h2>Page unavailable</h2><Link to={home}>Return to project jobs</Link></section>} /></Routes></div></div>
  </main></WorkspaceContext.Provider>;
}

function usePaging() {
  const [state, setState] = useState({ trail: [''], index: 0 });
  return { cursor: state.trail[state.index], number: state.index + 1, previous: () => setState(value => ({ ...value, index: Math.max(0, value.index - 1) })), next: (cursor: string) => setState(value => ({ trail: [...value.trail.slice(0, value.index + 1), cursor], index: value.index + 1 })) };
}
function Pager({ paging, page, label, busy }: { paging: ReturnType<typeof usePaging>; page?: Page<unknown>; label: string; busy: boolean }) {
  return <div className="pager"><span>Page {paging.number}{page ? ` · ${page.items.length} shown` : ''}{page && <small>{page.has_more ? 'More available' : 'End of list'}</small>}</span><div><button type="button" aria-label={`Previous ${label}`} disabled={paging.number === 1 || busy} onClick={paging.previous}>Previous</button><button type="button" aria-label={`Next ${label}`} disabled={!page?.has_more || !page.cursor || busy} onClick={() => { if (page?.cursor) paging.next(page.cursor); }}>Next</button></div></div>;
}
function ReadState({ error, loading, empty, children }: { error: unknown; loading: boolean; empty: boolean; children: ReactNode }) {
  const { fail } = useWorkspace();
  useEffect(() => { if (error) fail(error); }, [error, fail]);
  if (error) return <div className="notice" role="alert">{errorText(error)}</div>;
  if (loading) return <p className="empty" role="status">Loading…</p>;
  if (empty) return <p className="empty">Nothing here yet.</p>;
  return children;
}
function WorkerList({ compact = false }: { compact?: boolean }) {
  const { session, key, selectWorker } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'workers', paging.cursor], queryFn: ({ signal }) => session.api.workers({ limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className={compact ? 'worker-panel' : 'panel'}><div className="section-heading"><div><p className="eyebrow">People & agents</p><h2>{compact ? 'Workers' : 'Project workers'}</h2></div>{compact && <span className="small-label">Select inbox</span>}</div><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><ul className="worker-list">{query.data?.items.map(worker => <li key={worker.id}><button type="button" aria-label={`Open ${worker.name} inbox`} aria-pressed={session.worker?.id === worker.id} onClick={() => selectWorker(worker)}><span className="worker-avatar" aria-hidden="true">{worker.name.slice(0, 2).toUpperCase()}</span><span><strong>{worker.name}</strong><small>{worker.id}</small></span><span className="worker-arrow" aria-hidden="true">↗</span></button></li>)}</ul></ReadState><Pager paging={paging} page={query.data} label="workers" busy={query.isFetching} /></section>;
}
function Workers() { return <><div className="page-heading"><p className="eyebrow">Connected project</p><h2>Workers</h2><p>Select a worker to read their inbox and follow new messages.</p></div><WorkerList /></>; }
function Jobs() {
  const [status, setStatus] = useState('');
  const { session } = useWorkspace();
  const [assigned, setAssigned] = useState(false);
  return <><div className="page-heading"><p className="eyebrow">Work in motion</p><h2>Jobs</h2><p>Current work and its full context, one job at a time.</p></div><div className="filters"><label>Status<select aria-label="Job status" value={status} onChange={event => setStatus(event.target.value)}><option value="">All statuses</option><option value="active">Active</option><option value="parked">Parked</option><option value="blocked">Blocked</option><option value="completed">Completed</option></select></label>{session.worker && <label className="checkbox-label"><input type="checkbox" checked={assigned} onChange={event => setAssigned(event.target.checked)} />Assigned to {session.worker.name}</label>}</div><JobList key={`${status}/${assigned}`} status={status as JobSummary['status'] | ''} assigned={assigned} /></>;
}
function JobList({ status, assigned }: { status: JobSummary['status'] | ''; assigned: boolean }) {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'jobs', status, assigned, paging.cursor], queryFn: ({ signal }) => session.api.jobs({ limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}), ...(status ? { status } : {}), ...(assigned && session.worker ? { worker_id: session.worker.id } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><ul className="job-list">{query.data?.items.map(job => <li key={job.id}><div><span className={`badge status-${job.status}`}>{job.status}</span><Link className="job-title" to={`${home}/jobs/${encodeURIComponent(job.id)}`}>{job.title}</Link><p className="item-meta">{job.worker_id || 'Unassigned'}<span>Revision {job.revision}</span></p></div><Link className="detail-link" aria-label={`Read job ${job.title}`} to={`${home}/jobs/${encodeURIComponent(job.id)}`}>Read job <span aria-hidden="true">→</span></Link></li>)}</ul></ReadState><Pager paging={paging} page={query.data} label="jobs" busy={query.isFetching} /></section>;
}
function JobDetail() {
  const { jobId = '' } = useParams();
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'job', jobId], queryFn: ({ signal }) => session.api.job(jobId, signalFor(session, signal)), staleTime: Infinity });
  const job = query.data;
  return <><Link className="back-link" to={home}>← All jobs</Link><ReadState error={query.error} loading={query.isPending} empty={false}>{job && <><div className="page-heading"><span className={`badge status-${job.status}`}>{job.status}</span><h2>{job.title}</h2><p>{job.worker_id || 'Unassigned'} · Revision {job.revision}</p></div><section className="panel brief"><h3>Job brief</h3><p className="full-text">{job.brief || 'No brief supplied.'}</p><dl className="metadata"><div><dt>Job ID</dt><dd>{job.id}</dd></div><div><dt>Created</dt><dd>{time(job.created_at)}</dd></div><div><dt>Updated</dt><dd>{time(job.updated_at)}</dd></div></dl></section><JobCheckpointPanel key={jobId} jobId={jobId} jobRevision={job.revision} /></>}</ReadState></>;
}
function JobCheckpointPanel({ jobId, jobRevision }: { jobId: string; jobRevision: number }) {
  const { session, key } = useWorkspace();
  const [showHistory, setShowHistory] = useState(false);
  const query = useQuery({ queryKey: [...key, 'checkpoint-latest', jobId], queryFn: ({ signal }) => session.api.checkpointLatest(jobId, signalFor(session, signal)), staleTime: Infinity });
  const checkpoint = query.data?.checkpoint;
  return <section className="panel">
    <div className="section-heading"><div><p className="eyebrow">Job progress</p><h3>Latest checkpoint</h3></div></div>
    <ReadState error={query.error} loading={query.isPending} empty={false}>
      {query.data && <>
        {query.data.job_revision !== jobRevision && <p className="notice" role="status">This job changed while its details were being read. Refresh visible data to align the brief and checkpoint.</p>}
        {checkpoint ? <CheckpointContent checkpoint={checkpoint} /> : <p className="empty">No checkpoint has been recorded for this job.</p>}
        <button type="button" onClick={() => setShowHistory(value => !value)}>{showHistory ? 'Close checkpoint history' : 'Show checkpoint history'}</button>
        {showHistory && <CheckpointHistory key={jobId} jobId={jobId} />}
      </>}
    </ReadState>
  </section>;
}
function CheckpointContent({ checkpoint }: { checkpoint: Checkpoint }) {
  return <article className="brief">
    <p className="item-meta">Sequence {checkpoint.sequence} · Captured at job revision {checkpoint.job_revision}</p>
    <p className="full-text"><strong>Done</strong><br />{checkpoint.done}</p>
    <p className="full-text"><strong>Next</strong><br />{checkpoint.next}</p>
    <p className="full-text"><strong>Blocker</strong><br />{checkpoint.blocker || 'None'}</p>
    <div><strong>Evidence</strong>{checkpoint.evidence.length ? <ul>{checkpoint.evidence.map((item, index) => <li className="full-text" key={`${index}-${item}`}>{item}</li>)}</ul> : <p className="empty">No evidence references.</p>}</div>
    <dl className="metadata"><div><dt>Recorded by</dt><dd>{checkpoint.actor_id}</dd></div><div><dt>Worker</dt><dd>{checkpoint.worker_id || 'No worker attribution'}</dd></div><div><dt>Recorded</dt><dd><time dateTime={checkpoint.created_at}>{time(checkpoint.created_at)}</time></dd></div></dl>
  </article>;
}
function CheckpointHistory({ jobId }: { jobId: string }) {
  const { session, key } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'checkpoints', jobId, paging.cursor], queryFn: ({ signal }) => session.api.checkpoints(jobId, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <div className="brief"><h4>Checkpoint history</h4><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}>{query.data?.items.map(checkpoint => <CheckpointContent checkpoint={checkpoint} key={checkpoint.id} />)}</ReadState><Pager paging={paging} page={query.data} label="checkpoints" busy={query.isFetching} /></div>;
}
function ChooseWorker() { return <section className="panel empty"><h3>Choose a worker to open an inbox</h3><p>Select a worker from the list. Their incoming messages will stay connected as you browse.</p></section>; }
function Inbox() {
  const { session } = useWorkspace();
  return <><div className="page-heading"><p className="eyebrow">Conversations</p><h2>{session.worker ? `${session.worker.name} inbox` : 'Inbox'}</h2><p>Read incoming messages without changing their seen or acknowledged state.</p></div>{session.worker ? <InboxPage /> : <ChooseWorker />}</>;
}
function InboxPage() {
  const { session, key, home, ready } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'inbox', paging.cursor], queryFn: ({ signal }) => session.api.inbox(session.worker!.id, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), enabled: ready, staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={!ready || query.isPending} empty={!query.data?.items.length}><ul className="inbox-list">{query.data?.items.map(item => <InboxRow key={item.id} item={item} home={home} />)}</ul></ReadState><Pager paging={paging} page={query.data} label="inbox" busy={query.isFetching || !ready} /></section>;
}
function InboxRow({ item, home }: { item: InboxItem; home: string }) {
  return <li><div className="message-top"><strong>{item.sender_worker_id}</strong><time dateTime={item.created_at}>{time(item.created_at)}</time></div><Link className="message-preview" aria-label={`Read conversation from ${item.sender_worker_id}`} to={`${home}/inbox/${encodeURIComponent(item.conversation_id)}`}>{item.preview || 'Open conversation'}{item.preview_truncated ? '…' : ''}</Link><div className="item-meta"><span className={`badge ${item.seen ? 'neutral' : 'status-active'}`}>{item.seen ? 'Seen' : 'Unread'}</span><span>{item.acknowledged ? 'Acknowledged' : 'Not acknowledged'}</span></div></li>;
}
function Conversation() {
  const { session, home } = useWorkspace();
  const { conversationId = '' } = useParams();
  return <><Link className="back-link" to={`${home}/inbox`}>← Worker inbox</Link><div className="page-heading"><p className="eyebrow">{session.worker?.name ?? 'Worker inbox'}</p><h2>Conversation</h2><p>Reading leaves message state unchanged.</p></div>{session.worker ? <ConversationMessages key={conversationId} id={conversationId} /> : <ChooseWorker />}</>;
}
function ConversationMessages({ id }: { id: string }) {
  const { session, key } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'thread', id, paging.cursor], queryFn: ({ signal }) => session.api.thread(id, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><ol className="conversation-list">{query.data?.items.map(message => <li key={message.id}><div className="message-top"><strong>{message.sender_worker_id}<span className="message-direction"> to {message.recipient_worker_id}</span></strong><time dateTime={message.created_at}>{time(message.created_at)}</time></div><p className="full-text">{message.body}</p></li>)}</ol></ReadState><Pager paging={paging} page={query.data} label="messages" busy={query.isFetching} /></section>;
}

function useReviewSnapshot(): string | undefined {
  const [params] = useSearchParams();
  const values = params.getAll('snapshot');
  return values.length === 0 ? undefined : values.length === 1 ? values[0] : '';
}
function reviewLink(home: string, kind: 'pulls'|'runs', id: string | number, snapshot: string): string {
  return `${home}/reviews/${kind}/${encodeURIComponent(String(id))}?${new URLSearchParams({ snapshot })}`;
}
function githubLink(project: Project, path: string): string | undefined {
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(project.repository) || project.repository.split('/').some(part => part === '.' || part === '..')) return undefined;
  return `https://github.com/${project.repository.split('/').map(encodeURIComponent).join('/')}/${path}`;
}
function ReviewNotice() {
  return <p className="review-boundary">Stored review history. Attempt completion, current-comparison coverage and merge readiness are separate. These pages do not authorize a merge.<span className="review-count-key">Round counts: found / accepted / routed. Oldest to newest within each channel. Completed totals are historical, not coverage credit.</span></p>;
}
function ReviewProvenance({ value }: { value: ReviewSnapshot }) {
  const age = Math.max(0, Math.floor((Date.now() - Date.parse(value.observed_at)) / 3600000));
  return <section className="review-provenance" aria-label="Review observation"><dl><div><dt>Source</dt><dd>{value.source}</dd></div><div><dt>Observed</dt><dd><time dateTime={value.observed_at}>{time(value.observed_at)}</time><small>{age < 1 ? 'Less than an hour ago' : `${age} hours ago`} · reread does not refresh the source</small></dd></div><div><dt>Recorded</dt><dd><time dateTime={value.recorded_at}>{time(value.recorded_at)}</time></dd></div></dl><details><summary>Snapshot · revision {value.revision}</summary><code>{value.snapshot_id}</code></details></section>;
}
function RunPill({ run, snapshot, head }: { run: ReviewRunSummary; snapshot: string; head: string }) {
  const { home } = useWorkspace();
  const mismatch = !!head && !!run.head_sha && head !== run.head_sha;
  return <Link className={`review-round review-outcome-${run.outcome}`} to={reviewLink(home, 'runs', run.id, snapshot)} aria-label={`Open ${run.channel} run ${run.source_run_id}`}><span className="review-channel">{run.channel}</span><strong>{run.found}<small>found</small></strong><span>{run.outcome.replaceAll('_', ' ')}</span><small>{run.accepted} accepted · {run.routed} routed · {run.fixed} fixed</small>{!run.complete && <span className="review-warning">Incomplete record</span>}{!run.finalized && <span className="review-warning">Counts not finalized</span>}{!run.attributable && <span className="review-warning">Attribution unconfirmed</span>}{mismatch && <span className="review-warning">Different observed head</span>}</Link>;
}
function PullFacts({ pull }: { pull: ReviewPullSummary }) {
  return <div className="review-pull-facts"><span className={`badge ${pull.state === 'merged' ? 'neutral' : 'status-active'}`}>{pull.state}{pull.state === 'merged' ? ' · retained history' : ''}</span><span>Observed CI: {pull.ci_state}</span><span>Observed mergeability: {pull.mergeability}</span><span>{pull.run_count} runs · {pull.attempt_count} attempts</span>{!pull.complete && <span className="review-warning">Incomplete PR observation</span>}</div>;
}
function ReviewQueueView() {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const [continuationSnapshot, setContinuationSnapshot] = useState('current');
  const query = useQuery({ queryKey: [...key, 'reviews', paging.number === 1 ? 'current' : continuationSnapshot, 'queue', paging.cursor], queryFn: ({ signal }) => session.api.reviewQueue({ limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  const page = query.data;
  const navigation = { ...paging, next: (cursor: string) => { if (page) setContinuationSnapshot(page.snapshot_id); paging.next(cursor); } };
  return <><div className="page-heading"><p className="eyebrow">Review workspace</p><h2>Reviews</h2><p>Stored queue, review rounds and finding evidence.</p></div><ReviewNotice /><ReadState error={query.error} loading={query.isPending} empty={false}>{page && (page.configured ? <><ReviewProvenance value={page} /><section className="panel"><ul className="review-queue">{page.items.map(pull => <li key={pull.number}><div className="review-pull-heading"><span className="review-number">#{pull.number}</span><h3><Link to={reviewLink(home, 'pulls', pull.number, page.snapshot_id)}>{pull.title}</Link></h3></div><PullFacts pull={pull} /><p className="review-head">Observed head: <code>{pull.head_sha || 'Unknown'}</code></p><ReviewRunsView number={pull.number} snapshot={page.snapshot_id} head={pull.head_sha} attempts={pull.latest_attempts} expected={pull.run_count} /><Link className="detail-link" to={reviewLink(home, 'pulls', pull.number, page.snapshot_id)}>Read PR #{pull.number} history →</Link></li>)}</ul>{page.items.length === 0 && <p className="empty">No pull requests in this stored queue.</p>}<Pager paging={navigation} page={page} label="review queue" busy={query.isFetching} /></section></> : <section className="panel empty"><h3>Review workspace unconfigured</h3><p>No review observation has been recorded for this project.</p></section>)}</ReadState></>;
}
function ReviewAttempts({ attempts }: { attempts: ReviewAttemptSummary[] }) {
  return <section className="panel review-attempts"><details><summary>Attempts ({attempts.length})</summary>{attempts.length === 0 ? <p className="review-empty">No attempts recorded.</p> : <ul>{[...attempts].reverse().map(attempt => <li key={attempt.source_attempt_id}><div><span className="review-channel">{attempt.channel}</span><strong>{attempt.state.replaceAll('_', ' ')}</strong></div><div><small>Started</small>{attempt.started_at ? time(attempt.started_at) : 'Unknown'}</div><div><small>Duration</small>{attempt.duration_seconds === null ? 'Unknown' : `${attempt.duration_seconds}s`}</div><div><small>Head</small><code>{attempt.head_sha || 'Unknown'}</code></div><div><small>Source attempt / run</small><span>{attempt.source_attempt_id}</span><span>{attempt.source_run_id || 'No run attached'}</span></div></li>)}</ul>}</details></section>;
}
function ReviewPullView() {
  const { number: rawNumber = '' } = useParams();
  const number = /^[1-9][0-9]*$/u.test(rawNumber) ? Number(rawNumber) : NaN;
  const snapshot = useReviewSnapshot();
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot ?? 'current', 'pull', number], queryFn: ({ signal }) => session.api.reviewPull(number, snapshot, signalFor(session, signal)), staleTime: Infinity });
  const value = query.data;
  return <><Link className="back-link" to={`${home}/reviews`}>← Review queue</Link><ReviewNotice /><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <><div className="page-heading"><p className="eyebrow">Pull request #{value.pull.number}</p><h2>{value.pull.title}</h2>{githubLink(session.project, `pull/${value.pull.number}`) && <a href={githubLink(session.project, `pull/${value.pull.number}`)} target="_blank" rel="noopener noreferrer">Open PR #{value.pull.number} on GitHub ↗</a>}</div><ReviewProvenance value={value} /><section className="panel review-pull-summary"><PullFacts pull={value.pull} /><dl className="review-ref-grid"><div><dt>Observed head</dt><dd><code>{value.pull.head_sha || 'Unknown'}</code></dd></div><div><dt>Base</dt><dd>{value.pull.base_ref || 'Unknown'}<code>{value.pull.base_sha}</code></dd></div></dl></section><ReviewRunsView key={`${value.snapshot_id}:${number}`} number={number} snapshot={value.snapshot_id} head={value.pull.head_sha} attempts={value.attempts} expected={value.pull.run_count} detail /><ReviewAttempts attempts={value.attempts} /></>}</ReadState></>;
}
function roundAge(value: string): string {
  if (!value) return 'Date unknown';
  const minutes = Math.floor((Date.now() - Date.parse(value)) / 60000);
  if (minutes < 0) return 'Future date';
  if (minutes < 1) return 'Just now';
  if (minutes < 60) return `${minutes}m ago`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)}h ${minutes % 60}m ago`;
  return `${Math.floor(minutes / 1440)}d ${Math.floor(minutes % 1440 / 60)}h ago`;
}
function CompactRunPill({ run, snapshot, head }: { run: ReviewRunSummary; snapshot: string; head: string }) {
  const { home } = useWorkspace();
  const date = run.finished_at || run.started_at;
  const warnings = [
    !run.complete && 'Incomplete record', !run.finalized && 'Counts not finalized',
    !run.attributable && 'Attribution unconfirmed',
    (!head || !run.head_sha) ? 'Head association unknown' : head !== run.head_sha && 'Different observed head',
  ].filter(Boolean).join('. ');
  const description = `${run.found}/${run.accepted}/${run.routed} (found / accepted / routed). ${run.outcome.replaceAll('_', ' ')}. ${date ? `${run.finished_at ? 'Finished' : 'Started'} ${time(date)} (${date})` : 'Date unknown'}.${warnings ? ` ${warnings}.` : ''}`;
  return <Link className={`review-round-pill review-pill-${run.outcome}${run.outcome === 'completed' && run.accepted === 0 ? ' review-pill-zero' : ''}`} to={reviewLink(home, 'runs', run.id, snapshot)} title={description} aria-label={`Open ${run.channel} run ${run.source_run_id}: ${description}`}><strong>{run.found}/{run.accepted}/{run.routed}</strong>{run.outcome !== 'completed' && <span className="review-pill-state">{run.outcome.replaceAll('_', ' ')}</span>}{date ? <time dateTime={date}>{roundAge(date)}</time> : <span className="review-pill-age">Date unknown</span>}</Link>;
}
function ReviewLane({ channel, title, runs, attempts, snapshot, head, loading, failed }: { channel: string; title: string; runs: ReviewRunSummary[]; attempts: ReviewAttemptSummary[]; snapshot: string; head: string; loading: boolean; failed: boolean }) {
  const laneRuns = runs.filter(run => run.channel === channel);
  const attempt = [...attempts].reverse().find(item => item.channel === channel);
  const date = attempt?.finished_at || attempt?.started_at;
  return <section className="review-lane" aria-label={title}><div className="review-lane-heading"><h4>{title}</h4><span title="Historical completed outcomes; no coverage or policy credit">{loading ? 'Loading…' : failed ? 'History unavailable' : `${laneRuns.filter(run => run.outcome === 'completed').length} completed`}</span></div>{attempt && <p className="review-lane-attempt">Latest attempt: <strong>{attempt.state.replaceAll('_', ' ')}</strong>{date && <> · <time dateTime={date} title={`${time(date)} (${date})`}>{roundAge(date)}</time></>}</p>}{loading ? <p className="review-lane-empty" role="status">Loading retained rounds…</p> : failed ? <p className="review-lane-empty">Round history could not be loaded.</p> : laneRuns.length ? <div className="review-round-pills">{laneRuns.map(run => <CompactRunPill key={run.id} run={run} snapshot={snapshot} head={head} />)}</div> : <p className="review-lane-empty">No rounds recorded. Coverage is unknown.</p>}</section>;
}
function ReviewRunsView({ number, snapshot, head, attempts, expected, detail = false }: { number: number; snapshot: string; head: string; attempts: ReviewAttemptSummary[]; expected: number; detail?: boolean }) {
  const { session, key } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot, 'pull', number, 'runs', 'all-retained', 100, 'source-newest-first'], queryFn: ({ signal }) => session.api.reviewRuns(number, { snapshot, limit: 100 }, signalFor(session, signal)), enabled: expected > 0, staleTime: Infinity });
  // Reverse retained producer order, rather than sorting unknown or equal dates.
  const runs = [...(query.data?.items ?? [])].reverse();
  const lane = (channel: string, title: string) => <ReviewLane key={channel} channel={channel} title={title} runs={runs} attempts={attempts} snapshot={snapshot} head={head} loading={expected > 0 && query.isPending} failed={!!query.error} />;
  const otherChannels = [['manual', 'Manual reviews'], ['subagent', 'Subagent reviews']].filter(([channel]) => runs.some(run => run.channel === channel) || attempts.some(attempt => attempt.channel === channel));
  return <section className={detail ? 'panel review-history' : 'review-history'} aria-label={`PR #${number} review rounds`}>{detail && <div className="section-heading"><h3>Review rounds</h3><span className="small-label">All retained rounds · oldest to newest per channel</span></div>}{query.error && <ReadState error={query.error} loading={false} empty={false}>{null}</ReadState>}<div className="review-lanes">{lane('hosted', 'Hosted CodeRabbit')}{lane('cli', 'CLI CodeRabbit')}</div>{otherChannels.length > 0 && <section className="review-other-history"><h4>Other retained reviews</h4><div className="review-lanes">{otherChannels.map(([channel, title]) => lane(channel, title))}</div></section>}{query.data && (query.data.has_more || runs.length !== expected) && <p className="notice">This retained round list is incomplete. The visible rounds do not represent the full history.</p>}</section>;
}
function FindingView({ finding }: { finding: ReviewFinding }) {
  const { session } = useWorkspace();
  const route = finding.route;
  const commit = route?.fix_sha ?? finding.fix_sha;
  const proof = route?.proof_note ?? finding.proof_note;
  const resolved = route?.fixed_at ?? finding.resolved_at;
  return <li className="review-finding">
    <div className="review-finding-heading"><span className={`badge review-severity-${finding.severity}`}>{finding.severity}</span><span className="badge neutral">{finding.disposition}</span></div>
    <h3>{finding.title}</h3><p className="full-text">{finding.detail || 'No additional detail recorded.'}</p>
    <div className="review-decision"><h4>Decision</h4><p className="full-text">{finding.decision_reason || 'No decision reason recorded.'}</p><p className="item-meta">{finding.actor || 'Actor unknown'} · {finding.decided_at ? time(finding.decided_at) : 'Decision time unknown'}</p></div>
    {route && <div className="review-decision"><h4>Receiving PR</h4><p>{githubLink(session.project, `pull/${route.target_pull_number}`) ? <a href={githubLink(session.project, `pull/${route.target_pull_number}`)} target="_blank" rel="noopener noreferrer">PR #{route.target_pull_number}</a> : `PR #${route.target_pull_number}`} · {route.disposition} · {route.state}</p><p className="full-text">{route.reason || 'Awaiting receiving decision.'}</p><p className="item-meta">{route.actor || 'No receiving actor recorded'}{route.decided_at ? ` · ${time(route.decided_at)}` : ''}</p>{route.state === 'active' && (route.disposition === 'unadjudicated' || route.disposition === 'accepted' && !route.fix_sha) && <p className="notice">Receiving obligation remains pending.</p>}</div>}
    <div className="review-proof"><h4>{route ? 'Receiving fix proof' : 'Fix proof'}</h4>{commit ? <p>{githubLink(session.project, `commit/${commit}`) ? <a href={githubLink(session.project, `commit/${commit}`)} target="_blank" rel="noopener noreferrer"><code>{commit}</code> ↗</a> : <code>{commit}</code>}</p> : <p className="muted">{route?.disposition === 'rejected' ? 'Receiving finding rejected; no fix claimed.' : 'No fix commit recorded.'}</p>}<p className="full-text">{proof || 'No proof note recorded.'}</p><p className="item-meta">{resolved ? `Resolved: ${time(resolved)}` : 'Resolution time unknown'}</p></div>
  </li>;
}

function ReviewCoverageLimits({ limits }: { limits: string[] }) {
  return limits.length ? <section className="panel review-coverage"><h3>Source coverage limitations</h3><ul>{limits.map((limit, index) => <li key={index}>{limit}</li>)}</ul></section> : null;
}
function CapturedComparisonAssessment({ evidenceId }: { evidenceId: string }) {
  const [open, setOpen] = useState(false);
  const { key } = useWorkspace();
  const queryClient = useQueryClient();
  const assessmentKey = [...key, 'reviews', 'policy-assessment', evidenceId];
  function close() {
    void queryClient.cancelQueries({ queryKey: assessmentKey, exact: true });
    setOpen(false);
  }
  return <section className="panel review-assessment" aria-label="Captured comparison assessment"><div className="section-heading"><div><h3>Captured comparison policy</h3><p>Current project requirements for this captured comparison.</p></div><button type="button" aria-expanded={open} onClick={() => open ? close() : setOpen(true)}>{open ? 'Close assessment' : 'Assess captured comparison'}</button></div>{open && <CapturedComparisonResult evidenceId={evidenceId} />}</section>;
}
export function CapturedComparisonRefusal({ code }: { code: 'review_held' | 'review_capacity' }) {
  return <div className="notice" role="alert">{code === 'review_held'
    ? 'The project repository no longer matches this captured comparison. Ask an administrator to verify the repository binding.'
    : 'This assessment exceeds the supported review-history capacity. Ask an operator to address the retained history limit.'}</div>;
}
function CapturedComparisonResult({ evidenceId }: { evidenceId: string }) {
  const { session, key } = useWorkspace();
  // Unmounting consumes the request signal and removes this query immediately.
  // Every explicit reopen reads current policy, rather than showing cached proof.
  const query = useQuery({ queryKey: [...key, 'reviews', 'policy-assessment', evidenceId], queryFn: ({ signal }) => session.api.reviewPolicyAssessment(evidenceId, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  // Durable refusals need operator action. Authorization and cancellation keep
  // their existing ReadState/session handling, independent of the refusal code.
  if (query.error instanceof ApiError && !terminal(query.error) && (query.error.code === 'review_held' || query.error.code === 'review_capacity')) return <CapturedComparisonRefusal code={query.error.code} />;
  const value = query.data;
  return <ReadState error={query.error} loading={query.isPending} empty={false}>{value && <div className="review-assessment-result">{query.isFetching && <p role="status">Refreshing assessment…</p>}<dl className="review-ref-grid"><div><dt>Assessment state</dt><dd><span className="badge neutral">{value.state}</span></dd></div><div><dt>Policy revision</dt><dd>{value.policy_revision}</dd></div><div><dt>Captured target</dt><dd>PR #{value.anchor.pull_number}<code>{value.anchor.candidate_sha}</code></dd></div><div><dt>Captured source observed</dt><dd>{time(value.anchor.observed_at)}</dd></div><div><dt>Captured comparison base</dt><dd><code>{value.anchor.base_sha}</code></dd></div><div><dt>Policy evaluated</dt><dd>{time(value.evaluated_at)}</dd></div></dl>{value.incoming_routes_capacity && <p className="notice">Incoming route inspection reached its supported capacity; policy remains held.</p>}{(value.incoming_routes_pending ?? 0) > 0 && <p className="notice">{value.incoming_routes_pending} incoming routed finding obligation(s) remain pending.</p>}{value.state === 'unconfigured' ? <p className="notice">Project review requirements are unconfigured.</p> : <div className="review-assessment-channels">{value.channels.map(channel => <section key={channel.channel} aria-label={`${channel.channel === 'hosted' ? 'Hosted' : 'CLI'} assessment`}><h4>{channel.channel === 'hosted' ? 'Hosted' : 'CLI'}</h4><dl><div><dt>Covered</dt><dd>{channel.covered ? 'Yes' : 'No'}</dd></div><div><dt>Pending</dt><dd>{channel.pending ? 'Yes' : 'No'}</dd></div></dl></section>)}</div>}<p className="item-meta">This evaluates the captured comparison only. It does not refresh GitHub, assess CI or declare merge readiness.</p></div>}</ReadState>;
}
function ReviewRunView() {
  const { runId = '' } = useParams();
  const snapshot = useReviewSnapshot();
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot ?? 'current', 'run', runId], queryFn: ({ signal }) => session.api.reviewRun(runId, snapshot, signalFor(session, signal)), staleTime: Infinity });
  const value = query.data;
  const pull = useQuery({ queryKey: [...key, 'reviews', value?.snapshot_id ?? snapshot ?? 'current', 'pull', value?.pull_number ?? 'unknown'], queryFn: ({ signal }) => session.api.reviewPull(value!.pull_number, value!.snapshot_id, signalFor(session, signal)), enabled: !!value, staleTime: Infinity });
  const mismatch = !!value?.run.head_sha && !!pull.data?.pull.head_sha && value.run.head_sha !== pull.data.pull.head_sha;
  return <><Link className="back-link" to={value ? reviewLink(home, 'pulls', value.pull_number, value.snapshot_id) : `${home}/reviews`}>← {value ? `PR #${value.pull_number} history` : 'Review queue'}</Link><ReviewNotice /><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <><div className="page-heading"><p className="eyebrow">PR #{value.pull_number} · {value.run.channel} run</p><h2>Review findings</h2><p>{value.run.reviewer || 'Reviewer unknown'} · {value.run.scope}</p></div><ReviewProvenance value={value} /><section className="panel review-run-summary"><RunPill run={value.run} snapshot={value.snapshot_id} head={pull.data?.pull.head_sha ?? ''} /><dl className="review-ref-grid"><div><dt>Original run source</dt><dd>{value.run.source}</dd></div><div><dt>Run head</dt><dd><code>{value.run.head_sha || 'Unknown'}</code></dd></div><div><dt>Observed PR head</dt><dd><code>{pull.data?.pull.head_sha || 'Unknown'}</code></dd></div><div><dt>Started / finished</dt><dd>{value.run.started_at ? time(value.run.started_at) : 'Unknown'}<br />{value.run.finished_at ? time(value.run.finished_at) : 'Unknown'}</dd></div></dl>{mismatch && <p className="notice">This run belongs to a different head than the PR head in this observation. It does not establish coverage for that head.</p>}{(!value.run.complete || !value.run.attributable || !value.run.finalized) && <p className="notice">This run has incomplete, unattributed or unfinalized source evidence. Counts are historical observations.</p>}<ReadState error={pull.error} loading={pull.isPending} empty={false}>{!value.run.head_sha || !pull.data?.pull.head_sha ? <p className="review-warning">Head association is unknown.</p> : null}</ReadState></section>{value.run.source === 'native-executor' && /^review_evidence_[a-f0-9]{32}$/u.test(value.run.source_run_id) && <CapturedComparisonAssessment key={value.run.source_run_id} evidenceId={value.run.source_run_id} />}<ReviewCoverageLimits limits={value.run.coverage_limits} /><section className="panel review-findings"><div className="section-heading"><h3>Findings ({value.findings.length} recorded / {value.run.found} reported)</h3></div>{value.findings.length ? <ul>{value.findings.map(finding => <FindingView key={finding.source_finding_id} finding={finding} />)}</ul> : <p className="empty">No findings recorded for this run.{!value.run.complete ? ' The source record is incomplete.' : ''}</p>}</section></>}</ReadState></>;
}
