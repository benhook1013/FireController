import flameEmber from './flame-ember.svg?no-inline';
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { Link, NavLink, Route, Routes, useMatch, useNavigate, useParams } from 'react-router';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Api, ApiError } from './api';
import { refreshEventQueries } from './queries';
import type { InboxItem, JobSummary, LiveStatus, Page, Project, Worker } from './types';

const pageSize = 20;
type Session = { api: Api; project: Project; instanceId: string; nonce: string; controller: AbortController; worker: Worker | null };
type WorkspaceValue = { session: Session; key: readonly string[]; home: string; selectWorker(worker: Worker): void; fail(error: unknown): void; ready: boolean };
const WorkspaceContext = createContext<WorkspaceValue | null>(null);
function useWorkspace(): WorkspaceValue { const value = useContext(WorkspaceContext); if (!value) throw new Error('Workspace unavailable'); return value; }
function nonce(): string { return Array.from(crypto.getRandomValues(new Uint32Array(4)), value => value.toString(16).padStart(8, '0')).join(''); }
function signalFor(session: Session, signal: AbortSignal): AbortSignal { return AbortSignal.any([session.controller.signal, signal]); }
function terminal(error: unknown): boolean { return error instanceof ApiError && (error.status === 401 || error.status === 403 || error.code === 'unauthorized' || error.code === 'forbidden' || error.code === 'instance_mismatch'); }
function errorText(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401 || error.status === 403 || error.code === 'unauthorized' || error.code === 'forbidden') return 'Access ended. Connect again with a credential that can read this project.';
    if (error.code === 'instance_mismatch') return 'The service identity changed. Connect again to verify this project.';
    if (error.code === 'invalid_binding') return 'Enter a valid project ID and project credential.';
    if (error.status === 404) return 'This item is unavailable in the connected project.';
    if (error.code === 'connection_failed' || error.code === 'deadline_exceeded') return 'The service could not be reached in time. Try again when the connection is available.';
  }
  return 'This information could not be loaded. Try refreshing this view.';
}
function time(value: string): string { return new Date(value).toLocaleString(); }

export default function App() {
  const navigate = useNavigate();
  const match = useMatch('/projects/:projectId/*');
  const routeProject = match?.params.projectId;
  const queryClient = useQueryClient();
  const [session, setSession] = useState<Session | null>(null);
  const current = useRef<Session | null>(null);
  const pending = useRef<{ api: Api; controller: AbortController; projectId: string } | null>(null);
  const [notice, setNotice] = useState('');
  const [connecting, setConnecting] = useState(false);
  const clear = useCallback((message = '') => {
    pending.current?.controller.abort(); pending.current?.api.close(); pending.current = null;
    const old = current.current;
    current.current = null;
    old?.controller.abort(); old?.api.close();
    if (old) { void queryClient.cancelQueries({ queryKey: ['fc', old.nonce] }); queryClient.removeQueries({ queryKey: ['fc', old.nonce] }); }
    setSession(null); setConnecting(false); setNotice(message);
  }, [queryClient]);
  useEffect(() => {
    if (current.current && routeProject !== current.current.project.id) clear('Connect to open this project.');
    if (pending.current && routeProject && routeProject !== pending.current.projectId) clear('Connect to open this project.');
  }, [routeProject, clear]);
  useEffect(() => () => {
    pending.current?.controller.abort(); pending.current?.api.close();
    const old = current.current; old?.controller.abort(); old?.api.close();
    if (old) queryClient.removeQueries({ queryKey: ['fc', old.nonce] });
  }, [queryClient]);
  async function connect(projectId: string, token: string) {
    clear();
    let api: Api;
    try { api = new Api(token, projectId); } catch (error) { setNotice(errorText(error)); return; }
    const controller = new AbortController();
    const attempt = { api, controller, projectId };
    pending.current = attempt; setConnecting(true);
    try {
      const project = await api.connect(controller.signal);
      if (pending.current !== attempt || controller.signal.aborted) return;
      if (!api.instanceId) throw new ApiError('invalid_response', 'The service identity is unavailable.');
      const next: Session = { api, project, instanceId: api.instanceId, nonce: nonce(), controller, worker: null };
      pending.current = null; current.current = next; setSession(next); setConnecting(false);
      if (routeProject !== project.id) navigate(`/projects/${encodeURIComponent(project.id)}`);
    } catch (error) {
      api.close();
      if (pending.current === attempt) { pending.current = null; setConnecting(false); setNotice(errorText(error)); }
    }
  }
  function selectWorker(worker: Worker) {
    const old = current.current;
    if (!old) return;
    if (old.worker?.id !== worker.id) {
      old.controller.abort();
      void queryClient.cancelQueries({ queryKey: ['fc', old.nonce] });
      queryClient.removeQueries({ queryKey: ['fc', old.nonce] });
      const next = { ...old, worker, nonce: nonce(), controller: new AbortController() };
      current.current = next; setSession(next);
    }
    navigate(`/projects/${encodeURIComponent(old.project.id)}/inbox`);
  }
  const connected = session && routeProject === session.project.id;
  return <>
    <header className="mast"><div className="mast-inner"><Link className="brand" to={connected ? `/projects/${encodeURIComponent(session.project.id)}` : '/'}><Flame />FireController</Link><span className="mast-label">Project workspace</span></div></header>
    {connected ? <Workspace key={session.nonce} session={session} selectWorker={selectWorker} disconnect={() => { clear(); navigate('/'); }} fail={error => { if (terminal(error)) clear(errorText(error)); }} />
      : <Connect key={routeProject ?? 'root'} projectId={routeProject ?? ''} busy={connecting} notice={notice} onConnect={connect} cancel={() => clear('Connection cancelled.')} />}
    <footer className="footer"><span className="attribution">Powered by <a href="https://github.com/benhook1013/FireController" target="_blank" rel="noreferrer noopener">FireController</a></span><span>Read-only project workspace</span></footer>
  </>;
}

function Flame() { return <img className="flame" src={flameEmber} alt="" aria-hidden="true" />; }
function Connect({ projectId, busy, notice, onConnect, cancel }: { projectId: string; busy: boolean; notice: string; onConnect(project: string, token: string): void; cancel(): void }) {
  const [project, setProject] = useState(projectId);
  const [token, setToken] = useState('');
  function submit(event: FormEvent) { event.preventDefault(); const supplied = token; setToken(''); onConnect(project, supplied); }
  return <main className="connect-layout"><section className="connect-intro"><p className="eyebrow">Your project, in view</p><h1>Keep work<br /> moving.</h1><p>A clear view of your workers, jobs and conversations.</p><div className="intro-rule" /><p className="quiet-light">Connect to a project on this service to begin.</p></section>
    <section className="connect-card"><p className="eyebrow">FireController</p><h2>Connect to a project</h2><p className="muted">Use the project ID and a credential with read access.</p>
      {notice && <div className="notice" role="alert">{notice}</div>}
      <form onSubmit={submit} autoComplete="off"><label htmlFor="project-id">Project ID</label><input id="project-id" name="project-id" value={project} onChange={event => setProject(event.target.value)} required maxLength={128} spellCheck={false} autoCapitalize="none" disabled={busy} />
        <label htmlFor="project-credential">Project credential</label><input id="project-credential" name="project-credential" type="password" value={token} onChange={event => setToken(event.target.value)} required maxLength={4096} autoComplete="off" disabled={busy} />
        <p className="field-note">Kept only for this connection. Disconnect to clear access and project data.</p>
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
  const failRef = useRef(fail); failRef.current = fail;
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
    <nav className="navigation" aria-label="Project navigation"><NavLink to={home} end>Jobs</NavLink><NavLink to={`${home}/workers`}>Workers</NavLink><NavLink to={`${home}/inbox`}>Inbox</NavLink><span className="connection-note">Connected</span></nav>
    <div className="workspace-grid"><aside className="sidebar"><WorkerList compact /><section className="feed-card" aria-label="Inbox connection"><span className={`status-dot ${live === 'live' && session.worker ? 'online' : ''}`} /><strong role="status">{liveLabel}</strong><p>{session.worker ? session.worker.name : 'Select a worker to follow incoming messages.'}</p>{Boolean(feedError) && <><p className="error-copy">{errorText(feedError)}</p><button type="button" onClick={() => { setReady(false); setAttempt(value => value + 1); }}>Reconnect inbox</button></>}</section></aside>
      <div className="main-content"><Routes><Route path="/projects/:projectId" element={<Jobs />} /><Route path="/projects/:projectId/workers" element={<Workers />} /><Route path="/projects/:projectId/jobs/:jobId" element={<JobDetail />} /><Route path="/projects/:projectId/inbox" element={<Inbox />} /><Route path="/projects/:projectId/inbox/:conversationId" element={<Conversation />} /><Route path="*" element={<section className="panel"><h2>Page unavailable</h2><Link to={home}>Return to project jobs</Link></section>} /></Routes></div></div>
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
  return <><Link className="back-link" to={home}>← All jobs</Link><ReadState error={query.error} loading={query.isPending} empty={false}>{job && <><div className="page-heading"><span className={`badge status-${job.status}`}>{job.status}</span><h2>{job.title}</h2><p>{job.worker_id || 'Unassigned'} · Revision {job.revision}</p></div><section className="panel brief"><h3>Job brief</h3><p className="full-text">{job.brief || 'No brief supplied.'}</p><dl className="metadata"><div><dt>Job ID</dt><dd>{job.id}</dd></div><div><dt>Created</dt><dd>{time(job.created_at)}</dd></div><div><dt>Updated</dt><dd>{time(job.updated_at)}</dd></div></dl></section></>}</ReadState></>;
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
