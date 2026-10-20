import { useCallback, useEffect, useMemo, useRef, useState, useLayoutEffect } from 'react';
import type { FormEvent } from 'react';
import { Link, NavLink, Route, Routes, matchPath, useBlocker, useLocation, useMatch, useNavigate, useParams, useSearchParams } from 'react-router';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Api, ApiError, type BrowserSessionView } from './api';
import { WorkspaceContext, useWorkspace, signalFor, usePaging, Pager, ReadState, errorText } from './workspace';
import type { Session } from './workspace';
import { PublicSiteRouter } from './public-site';
import { BrowserActionMemory, MessageComposer, RecipientActions } from './browser-actions';
import { ProjectSettings } from './project-settings';
import { ProjectMap, ProjectMapEntryView, ProjectMapRevisionView, JobWorkstreamPanel } from './project-map';
import { WorkerCoordinationView, NotesView, NoteView, NoteRevisionView } from './coordination';
import { ContextualNotes } from './contextual-notes';
import { EditorialMarkdown } from './editorial-markdown';
import { githubRepositoryURL } from './github-links';
import { Delivery, DeliveryHero } from './delivery';
import { jobFilterSearch, readJobFilters, type JobFilters } from './job-navigation';
import { refreshEventQueries } from './queries';
import { ReviewQueueView, ReviewPullView, ReviewRunView } from './review-workspace';
import { PageSkeleton, WorkspaceSkeleton } from './loading';
import { browserSessionKey, clearBrowserSession, loadBrowserSession, renewedBrowserSessionBinding, saveBrowserSession, terminalBrowserSession, type BrowserSessionBinding } from './browser-session';
import { BrowserSessionRenewal, listenForSessionActivity } from './session-renewal';
import { applyPresentationRoot } from './presentation';
import { PresentationContext, usePresentation, useProjectHeading } from './presentation-context';
import { isCurrentProjectView, refreshProjectMetadata } from './project-metadata';
import { startMetadataAttempt } from './metadata-recovery';
import { createAppearanceAcceptor, refreshProjectView } from './appearance-refresh';
import type { Presentation } from './types';
import flameEmber from './flame-ember.svg?no-inline';
import type { Checkpoint, ChecklistItem, JobUpdate, InboxItem, LiveStatus, Project, Worker } from './types';

const pageSize = 20;
function nonce(): string { return Array.from(crypto.getRandomValues(new Uint32Array(4)), value => value.toString(16).padStart(8, '0')).join(''); }
function terminal(error: unknown): boolean { return error instanceof ApiError && terminalBrowserSession(error); }
function time(value: string): string { return new Date(value).toLocaleString(); }
const discardActionWarning = 'An action is still pending or its result is unknown. Keep this connection open to retry its original key and content. Leave this connection and discard that local recovery context?';

export function leavesActionProject(projectId: string, pathname: string): boolean {
  return matchPath('/projects/:projectId/*', pathname)?.params.projectId !== projectId;
}

export default function App() {
  const location = useLocation();
  return location.pathname === '/public' || location.pathname.startsWith('/public/') ? <PublicSiteRouter /> : <PrivateApp />;
}

// Presence controls only the initial loading shell. Validation, expiry and
// discarded-session cleanup stay in the effect, never in a render initializer.
function hasSavedBrowserBinding(): boolean {
  try { return Boolean(globalThis.sessionStorage?.getItem(browserSessionKey)); }
  catch { return false; }
}

function PrivateApp() {
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
  const [restoring, setRestoring] = useState(hasSavedBrowserBinding);
  const [appearance, setAppearance] = useState<Presentation>();
  const appearanceAcceptor = useRef<ReturnType<typeof createAppearanceAcceptor> | null>(null);
  const [metadataStatus, setMetadataStatus] = useState<LiveStatus>('connecting');
  const [refreshingSession, setRefreshingSession] = useState<Session | null>(null);
  const renewal = useRef<BrowserSessionRenewal<BrowserSessionView> | null>(null);
  const [metadataRetry, setMetadataRetry] = useState<{ lifetime: AbortController; attempt: number }>();
  const clear = useCallback((message = '') => {
    const expiring = pending.current?.api ?? current.current?.api;
    if (expiring) void expiring.endBrowserSession().catch(() => {});
    else if (saved.current) expireBinding(saved.current);
    clearBrowserSession(); saved.current = null; setResumable(null);
    pending.current?.controller.abort(); pending.current?.api.close(); pending.current = null;
    const old = current.current;
    current.current = null; appearanceAcceptor.current = null;
    old?.controller.abort(); old?.projectController.abort(); old?.api.close();
    if (old) { void queryClient.cancelQueries({ queryKey: ['fc', old.nonce] }); queryClient.removeQueries({ queryKey: ['fc', old.nonce] }); }
    setSession(null); setRefreshingSession(null); setAppearance(undefined); applyPresentationRoot(document.documentElement, document); setConnecting(false); setRestoring(false); setNotice(message);
  }, [queryClient]);
  // The data router asks before it unmounts this private workspace, including
  // Back navigation and transitions to the separate public application.
  const blocker = useBlocker(({ nextLocation }) => {
    const live = current.current;
    return Boolean(live && live.actions.unresolved > 0 && leavesActionProject(live.project.id, nextLocation.pathname));
  });
  useEffect(() => {
    if (blocker.state !== 'blocked') return;
    if (window.confirm(discardActionWarning)) { clear(); blocker.proceed(); }
    else blocker.reset();
  }, [blocker, clear]);
  useEffect(() => {
    if (current.current && routeProject !== current.current.project.id) clear('Connect to open this project.');
    if (pending.current && location.key !== pending.current.routeKey) clear('Connect to open this project.');
  }, [routeProject, location.key, clear]);
  useEffect(() => () => {
    pending.current?.controller.abort(); pending.current?.api.close(); pending.current = null;
    const old = current.current; old?.controller.abort(); old?.projectController.abort(); old?.api.close();
    applyPresentationRoot(document.documentElement, document);
    current.current = null;
    if (old) queryClient.removeQueries({ queryKey: ['fc', old.nonce] });
  }, [queryClient]);
  useEffect(() => {
    if (current.current || pending.current) return;
    const binding = saved.current?.projectID === routeProject ? saved.current : loadBrowserSession(routeProject, expireBinding);
    saved.current = binding; setResumable(binding); setRestoring(Boolean(binding));
    if (binding) void connect(binding.projectID, undefined, binding);
  }, [routeProject, reconnect]);
  async function connect(projectId: string, token?: string, restored?: BrowserSessionBinding) {
    if (!restored) clear();
    let api: Api;
    try { api = restored ? Api.resume(restored.projectID, restored.sessionID, restored.instanceID) : new Api(token ?? '', projectId); } catch (error) { setRestoring(false); setConnecting(false); setNotice(errorText(error)); return; }
    const controller = new AbortController();
    const attempt = { api, controller, projectId, routeKey: location.key };
    pending.current = attempt; setConnecting(true); setNotice('');
    const active = () => pending.current === attempt && !controller.signal.aborted && route.current.key === attempt.routeKey;
    try {
      const project = await api.connect(controller.signal);
      if (!active()) return;
      if (!api.instanceId) throw new ApiError('invalid_response', 'The service identity is unavailable.');
      let binding = restored;
      if (binding) {
        const verified = api.browserSession;
        const refreshed = verified && renewedBrowserSessionBinding(binding, verified);
        if (!refreshed) throw new ApiError('invalid_response', 'The service returned an invalid browser session.');
        binding = refreshed;
      }
      if (!binding) {
        const started = await api.startBrowserSession(controller.signal);
        if (!active()) return;
        binding = { version: 1, projectID: started.project_id, instanceID: started.instance_id, sessionID: started.session_id, expiresAt: started.expires_at, selectedworkerID: '' };
      }
      const presentationSnapshot = await api.presentation(controller.signal);
      if (!active()) return;
      const worker = binding.selectedworkerID ? await restoredWorker(api, binding.selectedworkerID, controller.signal) : null;
      if (!active()) return;
      binding = { ...binding, selectedworkerID: worker?.id ?? '' };
      if (!saveBrowserSession(binding)) throw new ApiError('browser_storage_unavailable', 'The browser session could not be retained safely.');
      saved.current = binding; setResumable(binding);
      const next: Session = { api, project, instanceId: api.instanceId!, nonce: nonce(), controller: new AbortController(), projectController: controller, presentationSnapshot, worker, binding, actions: new BrowserActionMemory(), renewalAttemptAt: restored ? undefined : performance.now() };
      pending.current = null; current.current = next;
      appearanceAcceptor.current = createAppearanceAcceptor(project.id, () => current.current?.api === api && current.current?.projectController === controller && !controller.signal.aborted, setAppearance);
      appearanceAcceptor.current(presentationSnapshot.presentation);
      setMetadataStatus('connecting'); setSession(next); setConnecting(false); setRestoring(false);
      if (routeProject !== project.id) navigate(`/projects/${encodeURIComponent(project.id)}`);
    } catch (error) {
      if (!restored) void api.endBrowserSession().catch(() => {});
      api.close();
      if (pending.current === attempt) {
        if (terminal(error)) clear(errorText(error));
        else { pending.current = null; setConnecting(false); setRestoring(false); setNotice(errorText(error)); }
      }
    }
  }
  function selectWorker(worker: Worker | null) {
    const old = current.current;
    if (!old) return;
    if ((old.worker?.id ?? '') !== (worker?.id ?? '')) {
      old.controller.abort();
      void queryClient.cancelQueries({ queryKey: ['fc', old.nonce] });
      queryClient.removeQueries({ queryKey: ['fc', old.nonce] });
      const binding = { ...old.binding, selectedworkerID: worker?.id ?? '' };
      if (!saveBrowserSession(binding)) {
        clear('The selected worker could not be retained. Enable session storage and connect again.');
        return;
      }
      saved.current = binding; setResumable(binding);
      const next = { ...old, worker, binding, nonce: nonce(), controller: new AbortController() };
      current.current = next; setSession(next);
    }
    if (worker && ((old.worker?.id ?? '') !== worker.id || conversationMatch?.params.projectId !== old.project.id)) {
      navigate(`/projects/${encodeURIComponent(old.project.id)}/inbox`);
    }
  }
  // This lifetime is project-bound: selecting an inbox worker replaces only
  // its content/message controller and does not restart the metadata feed.
  const projectLifetime = session?.projectController;
  const metadataAttempt = metadataRetry?.lifetime === projectLifetime ? metadataRetry?.attempt ?? 0 : 0;
  function reconnectProjectUpdates() {
    const live = current.current;
    if (!live || metadataStatus !== 'closed' || !isCurrentProjectView(live.project.id, route.current.pathname, live.projectController, projectLifetime)) return;
    setMetadataStatus('connecting');
    setMetadataRetry(previous => ({ lifetime: live.projectController, attempt: previous?.lifetime === live.projectController ? previous.attempt + 1 : 1 }));
  }
  useEffect(() => {
    if (!session || !projectLifetime) return;
    const api = session.api;
    const active = () => current.current?.api === api && current.current.projectController === projectLifetime && !projectLifetime.signal.aborted;
    const owner = new BrowserSessionRenewal({
      renew: signal => api.renewBrowserSession(AbortSignal.any([signal, projectLifetime.signal])),
      current: active,
      visible: () => document.visibilityState === 'visible',
      initialAttemptAt: session.renewalAttemptAt,
      success: value => {
        if (!active()) return;
        const live = current.current!;
        const binding = renewedBrowserSessionBinding(live.binding, value);
        if (!binding) return;
        if (!saveBrowserSession(binding)) { clear('The browser session could not be retained safely. Connect again.'); return; }
        // Keep the connection object and query identity stable. Persisting the
        // fresh expiry must not remount views or discard in-flight actions.
        live.binding = binding; saved.current = binding; setResumable(binding);
      },
      failure: error => { if (active() && terminal(error)) clear(errorText(error)); },
    });
    renewal.current = owner;
    const stop = listenForSessionActivity(owner, window, document);
    void owner.activity();
    return () => { if (renewal.current === owner) renewal.current = null; stop(); };
  }, [projectLifetime, clear]);
  useEffect(() => { void renewal.current?.activity(); }, [location.key, projectLifetime]);
  useEffect(() => {
    if (!session) return;
    const memory = session.actions;
    const guard = (event: BeforeUnloadEvent) => { if (memory.unresolved > 0) { event.preventDefault(); event.returnValue = ''; } };
    window.addEventListener('beforeunload', guard);
    return () => window.removeEventListener('beforeunload', guard);
  }, [projectLifetime]);
  useEffect(() => {
    const acceptAppearance = appearanceAcceptor.current;
    if (!session || !projectLifetime || !acceptAppearance || routeProject !== session.project.id) return;
    const api = session.api;
    const valid = () => isCurrentProjectView(session.project.id, route.current.pathname, projectLifetime, current.current?.projectController);
    const queryKey = () => { const live = current.current!; return ['fc', live.nonce, live.instanceId, live.project.id, live.worker?.id ?? '']; };
    const attempt = startMetadataAttempt(api, session.presentationSnapshot, {
      lifetime: projectLifetime.signal, current: valid, retry: metadataAttempt > 0,
      refresh: (page, signal) => refreshProjectMetadata(queryClient, queryKey(), page, signal),
      appearance: acceptAppearance, status: setMetadataStatus,
      failure: error => { if (terminal(error)) clear(errorText(error)); },
    });
    return () => attempt.cancel();
  }, [projectLifetime, metadataAttempt, routeProject, queryClient, clear]);
  async function refreshVisibleData() {
    const live = current.current;
    if (!live || live.controller.signal.aborted || refreshingSession === live) return;
    setRefreshingSession(live);
    const key = ['fc', live.nonce, live.instanceId, live.project.id, live.worker?.id ?? ''];
    const acceptAppearance = appearanceAcceptor.current;
    const active = () => current.current === live && !live.controller.signal.aborted && isCurrentProjectView(live.project.id, route.current.pathname, live.projectController, current.current?.projectController);
    try {
      const failures = await refreshProjectView(() => live.api.presentation(live.controller.signal), () => refreshEventQueries(queryClient, key, live.controller.signal), active, value => acceptAppearance?.(value));
      const failure = failures.find(terminal);
      if (active() && failure) clear(errorText(failure));
    } finally { setRefreshingSession(previous => previous === live ? null : previous); }
  }
  function disconnect() {
    const live = current.current;
    if (!live || live.actions.unresolved > 0 && !window.confirm(discardActionWarning)) return;
    clear(); navigate('/');
  }
  const connected = session && routeProject === session.project.id;
  const visibleAppearance = connected ? appearance : undefined;
  useLayoutEffect(() => { applyPresentationRoot(document.documentElement, document, visibleAppearance); }, [visibleAppearance]);
  return <PresentationContext.Provider value={visibleAppearance}>
    <header className="mast"><div className="mast-inner"><Link className="brand" to={connected ? `/projects/${encodeURIComponent(session.project.id)}` : '/'}><Flame />{visibleAppearance?.brand_name ?? 'FireController'}</Link><div className="mast-meta"><span className="mast-label">{connected ? `${visibleAppearance?.name ?? session.project.name} workspace` : 'Project workspace'}</span>{connected && <ProjectHeaderUtilities status={metadataStatus} refreshing={refreshingSession === session} refresh={() => void refreshVisibleData()} disconnect={disconnect} />}</div></div></header>
    {connected ? <Workspace key={session.nonce} session={session} metadataStatus={metadataStatus} reconnectProjectUpdates={reconnectProjectUpdates} selectWorker={selectWorker} fail={error => { if (terminal(error)) clear(errorText(error)); }} />
      : session || restoring || resumable ? <OpeningProject sidebar={Boolean(matchPath('/projects/:projectId/inbox/*', location.pathname))} busy={Boolean(session) || restoring || connecting} notice={notice} retry={() => setReconnect(value => value + 1)} cancel={() => clear('Connection cancelled.')} />
      : <Connect key={routeProject ?? 'root'} projectId={routeProject ?? ''} busy={connecting} notice={notice} onConnect={(project, token) => void connect(project, token)} cancel={() => clear('Connection cancelled.')} />}
    <footer className="footer"><span className="attribution">Powered by <a href="https://github.com/benhook1013/FireController" target="_blank" rel="noreferrer noopener">FireController</a></span></footer>
  </PresentationContext.Provider>;
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
export function OpeningProject({ busy, notice, retry, cancel, sidebar = false }: { busy: boolean; notice: string; retry(): void; cancel(): void; sidebar?: boolean }) {
  if (busy) return <WorkspaceSkeleton sidebar={sidebar} />;
  return <main className="opening-project"><section className="panel opening-project-card" aria-labelledby="opening-project-title">
    <h1 id="opening-project-title">Project connection unavailable</h1>
    <p className="notice" role="alert">{notice || 'The project could not be reached. Try again when the connection is available.'}</p>
    <div className="opening-project-actions"><button type="button" className="primary" onClick={retry}>Retry connection</button><button type="button" onClick={cancel}>Cancel connection</button></div>
  </section></main>;
}

function Connect({ projectId, busy, notice, onConnect, cancel }: { projectId: string; busy: boolean; notice: string; onConnect(project: string, token: string): void; cancel(): void }) {
  const [project, setProject] = useState(projectId);
  const [token, setToken] = useState('');
  function submit(event: FormEvent) { event.preventDefault(); const supplied = token; setToken(''); onConnect(project, supplied); }
  return <main className="connect-layout"><section className="connect-intro"><h1>Keep work<br /> moving.</h1><p>A clear view of your AI agents, their jobs, and your review and merge trains.</p><div className="intro-rule" /><p className="quiet-light">Connect to a project on this service to begin.</p></section>
    <section className="connect-card"><h2>Connect to a project</h2>
      {notice && <div className="notice" role="alert">{notice}</div>}
      <form onSubmit={submit} autoComplete="off"><label htmlFor="project-id">Project ID</label><input id="project-id" name="project-id" value={project} onChange={event => setProject(event.target.value)} required maxLength={128} spellCheck={false} autoCapitalize="none" disabled={busy} />
        <label htmlFor="project-credential">Project credential</label><input id="project-credential" name="project-credential" type="password" value={token} onChange={event => setToken(event.target.value)} required maxLength={4096} autoComplete="off" disabled={busy} />
        <p className="field-note">Stay signed in while you use the site. Sign in again after 24 hours away.</p>
        <button className="primary" type="submit" disabled={busy}>{busy ? 'Connecting…' : 'Connect to project'}</button>{busy && <button type="button" onClick={cancel}>Cancel connection</button>}
      </form></section></main>;
}

export function ProjectConnectionStatus({ status, connectedLabel = 'Connected' }: { status: LiveStatus; connectedLabel?: string }) {
  const label = status === 'live' ? connectedLabel : status === 'closed' ? 'Disconnected' : status === 'reconnecting' ? 'Reconnecting' : 'Connecting';
  return <span className={`connection-note connection-${status}`} role="status" aria-label={`Project updates: ${status === 'live' ? 'connected' : label.toLowerCase()}`}>{label}</span>;
}

export function ProjectHeaderUtilities({ status, refreshing, refresh, disconnect }: { status: LiveStatus; refreshing: boolean; refresh(): void; disconnect(): void }) {
  return <div className="header-utilities" role="group" aria-label="Project utilities"><ProjectConnectionStatus status={status} /><button type="button" aria-label="Refresh visible data" aria-busy={refreshing} disabled={refreshing} onClick={refresh}>{refreshing ? 'Refreshing…' : 'Refresh'}</button><button type="button" onClick={disconnect}>Disconnect</button></div>;
}

export function ProjectIdentityHero({ name, repository }: { name: string; repository: string }) {
  const href = githubRepositoryURL(repository);
  return <section className="hero project-identity"><div className="hero-project"><h1>{name}</h1><p className="repository">{href ? <a href={href} target="_blank" rel="noreferrer noopener">{repository}</a> : repository}</p></div></section>;
}

export function ProjectUpdateNotice({ status, reconnect }: { status: LiveStatus; reconnect(): void }) {
  if (status !== 'closed') return null;
  return <p className="notice" role="status">Project updates disconnected. Displayed project data may be stale. <button type="button" onClick={reconnect}>Reconnect project updates</button></p>;
}

function Workspace({ session, metadataStatus, reconnectProjectUpdates, selectWorker, fail }: { session: Session; metadataStatus: LiveStatus; reconnectProjectUpdates(): void; selectWorker(worker: Worker | null): void; fail(error: unknown): void }) {
  const presentation = usePresentation();
  const jobsHeading = useProjectHeading('jobs'), workersHeading = useProjectHeading('workers'), inboxHeading = useProjectHeading('inbox'), reviewsHeading = useProjectHeading('reviews'), mapHeading = useProjectHeading('map');
  const queryClient = useQueryClient();
  const key = useMemo(() => ['fc', session.nonce, session.instanceId, session.project.id, session.worker?.id ?? ''] as const, [session]);
  const home = `/projects/${encodeURIComponent(session.project.id)}`;
  const pathname = useLocation().pathname;
  const inboxArea = pathname === `${home}/inbox` || pathname.startsWith(`${home}/inbox/`);
  const [live, setLive] = useState<LiveStatus>('connecting');
  const [ready, setReady] = useState(false);
  const [feedError, setFeedError] = useState<unknown>(null);
  const [attempt, setAttempt] = useState(0);
  const [unresolved, setUnresolved] = useState(session.actions.unresolved);
  useEffect(() => session.actions.subscribe(() => setUnresolved(session.actions.unresolved)), [session.actions]);
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
  const value = useMemo(() => ({ session, key, home, selectWorker, fail: onFailure, ready }), [session, key, home, selectWorker, onFailure, ready]);
  const liveLabel = !session.worker ? 'Choose a worker' : live === 'live' ? 'Live inbox connected' : live === 'reconnecting' ? 'Reconnecting inbox…' : live === 'closed' ? 'Inbox disconnected' : 'Connecting inbox…';
  return <WorkspaceContext.Provider value={value}><main className="workspace">
    <DeliveryHero><ProjectIdentityHero name={presentation?.name ?? session.project.name} repository={session.project.repository} /></DeliveryHero>
    <nav className="navigation" aria-label="Project navigation"><NavLink to={home} end>Delivery</NavLink><NavLink to={`${home}/jobs`}>{jobsHeading}</NavLink><NavLink to={`${home}/workers`}>{workersHeading}</NavLink><NavLink to={`${home}/inbox`}>{inboxHeading}</NavLink><NavLink to={`${home}/reviews`}>{reviewsHeading}</NavLink><NavLink to={`${home}/map`}>{mapHeading}</NavLink>{session.api.browserCapabilities.can_edit_presentation && <NavLink to={`${home}/settings`}>Settings</NavLink>}</nav>
    <ProjectUpdateNotice status={metadataStatus} reconnect={reconnectProjectUpdates} />
    {unresolved > 0 && <p className="notice" role="status">{unresolved} pending or uncertain action{unresolved === 1 ? '' : 's'} retained in this connection. Return to the original worker and view to retry before reloading or disconnecting.</p>}
    <div className={inboxArea ? 'workspace-grid' : 'project-main'}>{inboxArea && <aside className="sidebar"><WorkerList compact /><section className="feed-card" aria-label="Inbox connection"><span className={`status-dot ${live === 'live' && session.worker ? 'online' : ''}`} /><strong role="status">{liveLabel}</strong><p>{session.worker ? session.worker.name : 'Select a worker to follow incoming messages.'}</p>{Boolean(feedError) && <><p className="error-copy">{errorText(feedError)}</p><button type="button" onClick={() => { setReady(false); setAttempt(value => value + 1); }}>Reconnect inbox</button></>}</section></aside>}
      <div className="main-content"><Routes><Route path="/projects/:projectId" element={<Delivery />} /><Route path="/projects/:projectId/jobs" element={<Jobs />} /><Route path="/projects/:projectId/settings" element={<ProjectSettings />} /><Route path="/projects/:projectId/workers" element={<Workers />} /><Route path="/projects/:projectId/workers/:workerId" element={<WorkerCoordinationView />} /><Route path="/projects/:projectId/notes" element={<NotesView />} /><Route path="/projects/:projectId/notes/:noteId" element={<NoteView />} /><Route path="/projects/:projectId/notes/:noteId/history/:revision" element={<NoteRevisionView />} /><Route path="/projects/:projectId/jobs/:jobId" element={<JobDetail />} /><Route path="/projects/:projectId/inbox" element={<Inbox />} /><Route path="/projects/:projectId/inbox/:conversationId" element={<Conversation />} /><Route path="/projects/:projectId/reviews" element={<ReviewQueueView />} /><Route path="/projects/:projectId/reviews/history" element={<ReviewQueueView key="history" history />} /><Route path="/projects/:projectId/reviews/pulls/:number" element={<ReviewPullView />} /><Route path="/projects/:projectId/reviews/runs/:runId" element={<ReviewRunView />} /><Route path="/projects/:projectId/map" element={<ProjectMap />} /><Route path="/projects/:projectId/map/entries/:entryId" element={<ProjectMapEntryView />} /><Route path="/projects/:projectId/map/entries/:entryId/history/:revision" element={<ProjectMapRevisionView />} /><Route path="*" element={<section className="panel"><h2>Page unavailable</h2><Link to={home}>Return to Delivery</Link></section>} /></Routes></div></div>
  </main></WorkspaceContext.Provider>;
}

function WorkerList({ compact = false }: { compact?: boolean }) {
  const presentation = usePresentation();
  const label = useProjectHeading('workers');
  const { session, key, home, selectWorker } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'workers', paging.cursor], queryFn: ({ signal }) => session.api.workers({ limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className={compact ? 'worker-panel' : 'panel'}><div className="section-heading"><div><p className="eyebrow">People & agents</p><h2>{compact ? label : presentation?.headings.workers ?? 'Project workers'}</h2></div>{compact && <span className="small-label">Select worker</span>}</div><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><ul className="worker-list">{query.data?.items.map(worker => <li key={worker.id}><button type="button" aria-label={compact ? `Select worker ${worker.name}` : `Open ${worker.name} inbox`} aria-pressed={compact ? session.worker?.id === worker.id : undefined} onClick={() => selectWorker(worker)}><span className="worker-avatar" aria-hidden="true">{worker.name.slice(0, 2).toUpperCase()}</span><span><strong>{worker.name}</strong><small>{worker.id}</small></span><span className="worker-arrow" aria-hidden="true">↗</span></button><Link className="worker-detail-link" to={`${home}/workers/${encodeURIComponent(worker.id)}`} aria-label={`Read ${worker.name} details`}>Worker details</Link></li>)}</ul></ReadState><Pager paging={paging} page={query.data} label="workers" busy={query.isFetching} /></section>;
}
function Workers() { const label = useProjectHeading('workers'); return <><div className="page-heading"><p className="eyebrow">Connected project</p><h2>{label}</h2><p>View worker details or open an inbox.</p></div><WorkerList /></>; }
export function Jobs() {
  const label = useProjectHeading('jobs');
  const [search, setSearch] = useSearchParams();
  const filters = readJobFilters(search);
  const select = (next: JobFilters) => setSearch(jobFilterSearch(next));
  return <><div className="page-heading"><h2>{label}</h2></div><div className="filters"><label>Status<select aria-label="Job status" value={filters.status} onChange={event => select({ ...filters, status: event.target.value as JobFilters['status'] })}><option value="open">Open jobs</option><option value="">All statuses</option><option value="active">Active</option><option value="parked">Parked</option><option value="blocked">Blocked</option><option value="completed">Completed</option></select></label><JobWorkerFilter selected={filters.workerID} select={workerID => select({ ...filters, workerID })} /></div><JobList key={`${filters.status}/${filters.workerID}`} filters={filters} /></>;
}
function JobList({ filters }: { filters: JobFilters }) {
  const { session, key, home } = useWorkspace();
  const { status, workerID } = filters;
  const search = jobFilterSearch(filters);
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'jobs', status, workerID, paging.cursor], queryFn: ({ signal }) => session.api.jobs({ limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}), ...(status ? { status } : {}), ...(workerID ? { worker_id: workerID } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><ul className="job-list">{query.data?.items.map(job => <li key={job.id}><div><span className={`badge status-${job.status}`}>{job.status}</span><Link className="job-title" to={`${home}/jobs/${encodeURIComponent(job.id)}${search}`}>{job.title}</Link><p className="item-meta">{job.worker_id || 'Unassigned'}<span>Revision {job.revision}</span></p></div><Link className="detail-link" aria-label={`Read job ${job.title}`} to={`${home}/jobs/${encodeURIComponent(job.id)}${search}`}>Read job <span aria-hidden="true">→</span></Link></li>)}</ul></ReadState><Pager paging={paging} page={query.data} label="jobs" busy={query.isFetching} /></section>;
}
function JobWorkerFilter({ selected, select }: { selected: string; select(workerID: string): void }) {
  const { session, key } = useWorkspace();
  const client = useQueryClient();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'workers', paging.cursor], queryFn: ({ signal }) => session.api.workers({ limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  const workers = query.data?.items ?? [];
  // Reuse already loaded page labels; an off-page URL identity needs no new read.
  const selectedWorker = selected ? client.getQueriesData<{ items: Worker[] }>({ queryKey: [...key, 'workers'] }).flatMap(([, page]) => page?.items ?? []).find(worker => worker.id === selected) : undefined;
  return <div className="job-worker-filter"><label>Worker<select aria-label="Job worker" value={selected} onChange={event => select(event.target.value)}><option value="">All workers</option>{selected && !workers.some(worker => worker.id === selected) && <option value={selected}>{selectedWorker?.name ?? selected}</option>}{workers.map(worker => <option key={worker.id} value={worker.id}>{worker.name}</option>)}</select></label>{selected && <button className="worker-filter-clear" type="button" aria-label="Show jobs for all workers" onClick={() => select('')}>×</button>}{(paging.number > 1 || query.data?.has_more) && <div className="worker-option-pages"><span>Workers page {paging.number}</span><button type="button" disabled={paging.number === 1 || query.isFetching} onClick={paging.previous}>Previous workers</button><button type="button" disabled={!query.data?.has_more || !query.data.cursor || query.isFetching} onClick={() => { if (query.data?.cursor) paging.next(query.data.cursor); }}>More workers</button></div>}{query.error && <span className="error-copy" role="alert">Worker choices could not be loaded.</span>}</div>;
}

export function JobDetail() {
  const { jobId = '' } = useParams();
  const [search] = useSearchParams();
  const returnSearch = jobFilterSearch(readJobFilters(search));
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'job', jobId], queryFn: ({ signal }) => session.api.job(jobId, signalFor(session, signal)), staleTime: Infinity });
  const job = query.data;
  return <><Link className="back-link" to={`${home}/jobs${returnSearch}`}>← Back to jobs</Link><ReadState error={query.error} loading={query.isPending} empty={false}>{job && <><div className="page-heading"><span className={`badge status-${job.status}`}>{job.status}</span><h2>{job.title}</h2><p>{job.worker_id ? <Link to={`${home}/workers/${encodeURIComponent(job.worker_id)}`}>{job.worker_id}</Link> : 'Unassigned'} · Revision {job.revision}</p></div><ContextualNotes key={`notes-${jobId}`} jobId={jobId} /><section className="panel brief"><h3>Job brief</h3><EditorialMarkdown>{job.brief || 'No brief supplied.'}</EditorialMarkdown><dl className="metadata"><div><dt>Job ID</dt><dd>{job.id}</dd></div><div><dt>Created</dt><dd>{time(job.created_at)}</dd></div><div><dt>Updated</dt><dd>{time(job.updated_at)}</dd></div></dl></section><JobWorkstreamPanel key={`workstream-${jobId}`} jobId={jobId} /><JobChecklistPanel key={`checklist-${jobId}`} jobId={jobId} jobRevision={job.revision} /><JobCheckpointPanel key={jobId} jobId={jobId} jobRevision={job.revision} /><JobUpdatesPanel key={`updates-${jobId}`} jobId={jobId} jobRevision={job.revision} /></>}</ReadState></>;
}
export function ChecklistItems({ items }: { items: ChecklistItem[] }) {
  return items.length ? <ol>{items.map(item => <li key={item.id}><span className="badge neutral">{item.done ? 'Completed' : 'To do'}</span><EditorialMarkdown>{item.text}</EditorialMarkdown></li>)}</ol> : <p className="empty">No checklist items.</p>;
}
function JobChecklistPanel({ jobId, jobRevision }: { jobId: string; jobRevision: number }) {
  const { session, key } = useWorkspace();
  const [showHistory, setShowHistory] = useState(false);
  const query = useQuery({ queryKey: [...key, 'checklist-current', jobId], queryFn: ({ signal }) => session.api.jobChecklist(jobId, signalFor(session, signal)), staleTime: Infinity });
  const checklist = query.data;
  return <section className="panel" aria-label="Current job checklist"><div className="section-heading"><div><p className="eyebrow">Job progress</p><h3>Current checklist</h3></div></div><ReadState error={query.error} loading={query.isPending} empty={false}>{checklist && <>
    <div className="brief">
      <p className="item-meta">Checklist revision {checklist.revision} · Job revision {checklist.job_revision}</p>
      {checklist.job_revision !== jobRevision && <p className="notice" role="status">This job changed while its details were being read. Refresh visible data to align the brief and checklist.</p>}
      <ChecklistItems items={checklist.items} />
      <button type="button" aria-expanded={showHistory} onClick={() => setShowHistory(value => !value)}>{showHistory ? 'Close checklist history' : 'Show checklist history'}</button>
    </div>
    {showHistory && <JobChecklistHistory jobId={jobId} jobRevision={jobRevision} checklistRevision={checklist.revision} />}
  </>}</ReadState></section>;
}
function JobChecklistHistory({ jobId, jobRevision, checklistRevision }: { jobId: string; jobRevision: number; checklistRevision: number }) {
  const { session, key } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'checklist-history', jobId, paging.cursor], queryFn: ({ signal }) => session.api.jobChecklistHistory(jobId, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  return <div className="brief" aria-label="Checklist revision history"><h4>Checklist history</h4><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}>{query.data?.items.map(entry => <article className="brief" key={entry.revision}>
    <p className="item-meta">Checklist revision {entry.revision} · Recorded against job revision {entry.job_revision}</p>
    {(entry.job_revision > jobRevision || entry.revision > checklistRevision) && <p className="notice" role="status">The job or checklist changed while its details were being read. Refresh visible data to align this view.</p>}
    <ChecklistItems items={entry.items} /><dl className="metadata"><div><dt>Recorded by</dt><dd>{entry.actor_id}</dd></div><div><dt>Worker</dt><dd>{entry.worker_id || 'No worker attribution'}</dd></div><div><dt>Recorded</dt><dd><time dateTime={entry.created_at}>{time(entry.created_at)}</time></dd></div></dl>
  </article>)}</ReadState><Pager paging={paging} page={query.data} label="checklist history" busy={query.isFetching} /></div>;
}
function JobUpdatesPanel({ jobId, jobRevision }: { jobId: string; jobRevision: number }) {
  const [open, setOpen] = useState(false);
  return <section className="panel" aria-label="Job narrative updates"><div className="section-heading"><div><p className="eyebrow">Recorded progress</p><h3>Narrative updates</h3></div><button type="button" aria-expanded={open} onClick={() => setOpen(value => !value)}>{open ? 'Close updates' : 'Show updates'}</button></div>{open && <JobUpdates jobId={jobId} jobRevision={jobRevision} />}</section>;
}
export function JobUpdateContent({ update, jobRevision }: { update: JobUpdate; jobRevision: number }) {
  return <article className="brief"><p className="item-meta">Update {update.sequence} · Recorded against job revision {update.job_revision}</p>
    {update.job_revision > jobRevision && <p className="notice" role="status">The job changed while its details were being read. Refresh visible data to align the brief and updates.</p>}
    <EditorialMarkdown>{update.body}</EditorialMarkdown><div><strong>Evidence</strong>{update.evidence.length ? <ul>{update.evidence.map((reference, index) => <li className="full-text" key={`${index}-${reference}`}>{reference}</li>)}</ul> : <p className="empty">No evidence references.</p>}</div>
    <dl className="metadata"><div><dt>Recorded by</dt><dd>{update.actor_id}</dd></div><div><dt>Worker</dt><dd>{update.worker_id || 'No worker attribution'}</dd></div><div><dt>Recorded</dt><dd><time dateTime={update.created_at}>{time(update.created_at)}</time></dd></div></dl></article>;
}
function JobUpdates({ jobId, jobRevision }: { jobId: string; jobRevision: number }) {
  const { session, key } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'job-updates', jobId, paging.cursor], queryFn: ({ signal }) => session.api.jobUpdates(jobId, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  return <div className="brief"><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}>{query.data?.items.map(update => <JobUpdateContent key={update.id} update={update} jobRevision={jobRevision} />)}</ReadState><Pager paging={paging} page={query.data} label="updates" busy={query.isFetching} /></div>;
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
export function CheckpointContent({ checkpoint }: { checkpoint: Checkpoint }) {
  return <article className="brief">
    <p className="item-meta">Sequence {checkpoint.sequence} · Captured at job revision {checkpoint.job_revision}</p>
    <div><strong>Done</strong><EditorialMarkdown>{checkpoint.done}</EditorialMarkdown></div>
    <div><strong>Next</strong><EditorialMarkdown>{checkpoint.next}</EditorialMarkdown></div>
    <div><strong>Blocker</strong><EditorialMarkdown>{checkpoint.blocker || 'None'}</EditorialMarkdown></div>
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
export function inboxWorkerSelector(params: URLSearchParams): string | undefined {
  const values=params.getAll('worker_id');
  if (!values.length) return '';
  return values.length===1 && /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/u.test(values[0]) ? values[0] : undefined;
}
export function Inbox() {
  const [params]=useSearchParams();
  const workerId=inboxWorkerSelector(params);
  const {session,key,selectWorker}=useWorkspace();
  const worker=useQuery({queryKey:[...key,'worker',workerId ?? 'invalid'],queryFn:({signal})=>session.api.worker(workerId!,signalFor(session,signal)),enabled:!!workerId && session.worker?.id!==workerId,staleTime:Infinity});
  useEffect(()=>{
    if (workerId && worker.data?.id===workerId && worker.data.project_id===session.project.id && session.worker?.id!==workerId && !session.controller.signal.aborted) selectWorker(worker.data);
  },[workerId,worker.data,session,selectWorker]);
  if (workerId===undefined) return <p className="notice" role="alert">The inbox worker link is invalid. Choose a worker from the inbox list.</p>;
  if (workerId && session.worker?.id!==workerId) return <ReadState error={worker.error} loading={worker.isPending || !!worker.data} empty={false}>{null}</ReadState>;
  return <SelectedInbox />;
}
function SelectedInbox() {
  const presentation = usePresentation();
  const label = useProjectHeading('inbox');
  const { session } = useWorkspace();
  return <><div className="page-heading"><p className="eyebrow">Conversations</p><h2>{session.worker ? presentation?.headings.inbox ? `${session.worker.name} · ${label}` : `${session.worker.name} inbox` : label}</h2><p>Read incoming messages without changing their seen or acknowledged state.</p></div>{session.worker ? <><ContextualNotes key={`notes-${session.worker.id}`} workerId={session.worker.id} /><MessageComposer /><InboxPage /></> : <ChooseWorker />}</>;
}
function InboxPage() {
  const { session, key, home, ready } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'inbox', paging.cursor], queryFn: ({ signal }) => session.api.inbox(session.worker!.id, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), enabled: ready, staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={!ready || query.isPending} empty={!query.data?.items.length}><ul className="inbox-list">{query.data?.items.map(item => <InboxRow key={item.id} item={item} home={home} />)}</ul></ReadState><Pager paging={paging} page={query.data} label="inbox" busy={query.isFetching || !ready} /></section>;
}
function InboxRow({ item, home }: { item: InboxItem; home: string }) {
  return <li><div className="message-top"><strong>{item.sender_worker_id}</strong><time dateTime={item.created_at}>{time(item.created_at)}</time></div><Link className="message-preview" aria-label={`Read conversation from ${item.sender_worker_id}`} to={`${home}/inbox/${encodeURIComponent(item.conversation_id)}`}>{item.preview || 'Open conversation'}{item.preview_truncated ? '…' : ''}</Link><div className="item-meta"><span className={`badge ${item.seen ? 'neutral' : 'status-active'}`}>{item.seen ? 'Seen' : 'Unread'}</span><span>{item.acknowledged ? 'Acknowledged' : 'Not acknowledged'}</span></div><RecipientActions item={item} /></li>;
}
function Conversation() {
  const { session, home } = useWorkspace();
  const { conversationId = '' } = useParams();
  return <><Link className="back-link" to={`${home}/inbox`}>← Worker inbox</Link><div className="page-heading"><p className="eyebrow">{session.worker?.name ?? 'Worker inbox'}</p><h2>Conversation</h2><p>Reading leaves message state unchanged.</p></div>{session.worker ? <ConversationMessages key={conversationId} id={conversationId} /> : <ChooseWorker />}</>;
}
export function ConversationMessages({ id }: { id: string }) {
  const { session, key } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'thread', id, paging.cursor], queryFn: ({ signal }) => session.api.thread(id, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><ol className="conversation-list">{query.data?.items.map(message => <li key={message.id}><div className="message-top"><strong>{message.sender_worker_id}<span className="message-direction"> to {message.recipient_worker_id}</span></strong><time dateTime={message.created_at}>{time(message.created_at)}</time></div><EditorialMarkdown>{message.body}</EditorialMarkdown><MessageComposer reply={message} /></li>)}</ol></ReadState><Pager paging={paging} page={query.data} label="messages" busy={query.isFetching} /></section>;
}
