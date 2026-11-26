import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { Api, type BrowserSessionView } from './api';
import { ProjectSwitcher, browserProjectSwitchSafe, verifyProjectSwitch } from './App';
import { browserRecoveryKey, browserSessionKey, forgetBrowserSession, loadBrowserSession, saveBrowserSession, type BrowserSessionBinding } from './browser-session';
import { projectHome } from './project-address';

const instance = 'instance_synthetic', sourceID = 'a'.repeat(32), targetID = 'b'.repeat(32);
const projects = [
  { id: 'project_one', handle: 'one', name: 'One <literal>' },
  { id: 'project_two', handle: 'two', name: 'Two' },
];
const clients: Api[] = [];
const client = (api: Api) => { clients.push(api); return api; };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const failure = (status: number) => json({ error: { code: status === 403 ? 'forbidden' : 'unauthorized', message: 'PRIVATE_SERVER_DETAIL' }, request_id: 'synthetic_request' }, status);
function session(selector = sourceID, projectID = projects[0].id, admin = true): BrowserSessionView {
  return { session_id: selector, project_id: projectID, instance_id: instance, expires_at: '2026-11-22T00:00:00Z', allow_writes: admin, can_edit_presentation: admin, projects: structuredClone(projects) };
}
function project(id: string) { return { ...projects.find(value => value.id === id)!, github_repository_id: 1, repository: 'synthetic/repository' }; }
function presentation(id: string) { return { presentation: { project_id: id, revision: 0, name: project(id).name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' }, event_cursor: 'synthetic_cursor' }; }
function fixture() {
  vi.stubGlobal('location', { origin: 'https://synthetic.example' });
  const cookies = new Map<string, BrowserSessionView>([[sourceID, session()]]);
  const state: { switchMode: 'success' | 'lost' | 'denied'; response?: (value: BrowserSessionView, phase: string) => unknown; targetAdmin: boolean } = { switchMode: 'success', targetAdmin: false };
  const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input)), headers = new Headers(init?.headers);
    const selector = headers.get('X-FC-Browser-Session') ?? '';
    const projectID = url.pathname.split('/')[4];
    if (url.pathname.endsWith('/switch')) {
      if (state.switchMode === 'denied') return failure(403);
      const body = JSON.parse(String(init?.body));
      const target = session(body.session_id, body.project_id, state.targetAdmin);
      cookies.set(body.session_id, target);
      if (state.switchMode === 'lost') throw new TypeError('PRIVATE_LOST_BODY');
      return json(state.response?.(target, 'switch') ?? target);
    }
    if (init?.method === 'DELETE') { cookies.delete(selector); return json({ session_id: selector, logged_out: true }); }
    if (init?.method === 'POST') {
      const create = !url.pathname.endsWith('/renew');
      const value = create ? session(JSON.parse(String(init.body)).session_id) : cookies.get(selector)!;
      cookies.set(value.session_id, value);
      return json(state.response?.(value, create ? 'create' : 'renew') ?? value);
    }
    if (!headers.has('Authorization') && !cookies.has(selector)) return failure(401);
    if (url.pathname.endsWith('/instance')) return json({ instance_id: instance });
    if (url.pathname.endsWith('/browser-session')) {
      const value = cookies.get(selector)!;
      return json(state.response?.(value, 'current') ?? value);
    }
    if (url.pathname.endsWith('/presentation')) return json(presentation(projectID));
    if (projectID === projects[0].id || projectID === projects[1].id) return json(project(projectID));
    throw new Error(`Unexpected synthetic request: ${url.pathname}`);
  });
  vi.stubGlobal('fetch', fetcher);
  return { state, fetcher, cookies };
}
async function source() { const api = client(Api.resume(projects[0].id, sourceID, instance)); await api.connect(); return api; }
const target = () => client(Api.resume(projects[1].id, targetID, instance));
function storage() {
  const values = new Map<string, string>();
  return { values, getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); }, removeItem: (key: string) => { values.delete(key); } };
}
afterEach(() => { for (const api of clients.splice(0)) api.close(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('browser project catalog', () => {
  it('opts the UI exchange into switching while preserving the legacy request default', async () => {
    const f = fixture();
    for (const optIn of [false, true]) {
      const api = client(new Api('SYNTHETIC_CREDENTIAL_NOT_RETAINED', projects[0].id));
      await api.connect();
      const value = await api.startBrowserSession(undefined, optIn);
      const [, init] = f.fetcher.mock.calls.filter(([, request]) => request?.method === 'POST').at(-1)!;
      expect(JSON.parse(String(init?.body))).toEqual({ session_id: value.session_id, ...(optIn ? { project_switching: true } : {}) });
      expect(value.projects).toEqual(projects);
      expect(JSON.stringify(api)).not.toContain('SYNTHETIC_CREDENTIAL_NOT_RETAINED');
    }
  });

  it('accepts absent and empty catalogs, and eight bounded options on create/current/renew/switch', async () => {
    const f = fixture(), api = await source();
    const eight = [...projects, ...Array.from({ length: 6 }, (_, index) => ({ id: `project_${index}`, handle: `project-${index}`, name: 'é'.repeat(100) }))];
    for (const options of [undefined, [], eight]) {
      f.state.response = value => { const { projects: _projects, ...legacy } = value; return options === undefined ? legacy : { ...legacy, projects: options }; };
      for (const phase of ['create', 'current', 'renew', 'switch']) {
        let value: BrowserSessionView | undefined;
        if (phase === 'create') { const fresh = client(new Api('SYNTHETIC_CREDENTIAL', projects[0].id)); await fresh.connect(); value = await fresh.startBrowserSession(undefined, true); }
        if (phase === 'current') { const fresh = client(Api.resume(projects[0].id, sourceID, instance)); await fresh.connect(); value = fresh.browserSession; }
        if (phase === 'renew') value = await api.renewBrowserSession();
        if (phase === 'switch') {
          // Restore the source catalog; the returned target may independently
          // lose switching access or expose a different bounded selection.
          f.state.response = (reply, stage) => stage === 'current' ? reply : options === undefined ? (({ projects: _projects, ...legacy }) => legacy)(reply) : { ...reply, projects: options };
          await api.connect(); value = await api.switchBrowserSession(projects[1].id, targetID);
        }
        expect(value?.projects).toEqual(options);
      }
    }
  });

  const malformedCatalogs = [
    null, {}, 'projects', Array.from({ length: 9 }, (_, index) => ({ id: `p_${index}`, handle: `p-${index}`, name: 'Name' })),
    [projects[0], projects[0]], [projects[0], { ...projects[1], handle: projects[0].handle }],
    [{ ...projects[0], id: '../project' }], [{ ...projects[0], id: 'x'.repeat(129) }],
    [{ ...projects[0], handle: 'Uppercase' }], [{ ...projects[0], handle: 'bad/handle' }], [{ ...projects[0], handle: 'trailing-' }], [{ ...projects[0], handle: 'x'.repeat(64) }],
    [{ ...projects[0], name: '' }], [{ ...projects[0], name: ' padded' }], [{ ...projects[0], name: 'é'.repeat(101) }], [{ ...projects[0], name: '\ud800' }],
    [{ id: projects[0].id, name: projects[0].name }], [{ ...projects[0], private_record: 'NOT_ALLOWED' }],
  ];
  it.each(['create', 'current', 'renew', 'switch'])('strictly rejects malformed %s catalogs without broadening the source', async phase => {
    const f = fixture(), api = await source();
    for (const projects of malformedCatalogs) {
      f.state.response = (value, stage) => stage === phase ? { ...value, projects } : value;
      let operation: Promise<unknown>;
      if (phase === 'create') { const fresh = client(new Api('SYNTHETIC_CREDENTIAL', 'project_one')); await fresh.connect(); operation = fresh.startBrowserSession(undefined, true); }
      else if (phase === 'current') operation = client(Api.resume('project_one', sourceID, instance)).connect();
      else operation = phase === 'renew' ? api.renewBrowserSession() : api.switchBrowserSession('project_two', targetID);
      await expect(operation).rejects.toMatchObject({ code: 'invalid_response' });
      expect(api.browserCapabilities).toEqual({ allow_writes: true, can_edit_presentation: true });
    }
  });

  it('does not share mutable catalog objects with consumers and replaces them on renewal', async () => {
    const f = fixture(), api = await source();
    api.browserSession!.projects![0].id = 'foreign_project';
    api.browserSession!.projects!.push({ id: 'foreign', handle: 'foreign', name: 'Foreign' });
    expect(api.browserSession?.projects).toEqual(projects);
    f.state.response = value => ({ ...value, projects: [projects[0]] });
    const renewed = await api.renewBrowserSession(); renewed.projects!.push(projects[1]);
    expect(api.browserSession?.projects).toEqual([projects[0]]);
    const before = f.fetcher.mock.calls.length;
    await expect(api.switchBrowserSession(projects[1].id, targetID)).rejects.toMatchObject({ code: 'invalid_request' });
    expect(f.fetcher.mock.calls).toHaveLength(before);
  });
});

describe('verified project handoff', () => {
  it.each(['success', 'lost'] as const)('uses only exact cookie-bound target IDs and normal API resume after a %s response', async mode => {
    const f = fixture(), api = await source(), next = target(); f.state.switchMode = mode; f.fetcher.mockClear();
    const opened = await verifyProjectSwitch(api, next, targetID, new AbortController().signal);
    expect(opened.project.id).toBe(projects[1].id); expect(projectHome(opened.project)).toBe('/projects/two');
    expect(opened.presentationSnapshot.presentation.project_id).toBe(projects[1].id);
    expect(next.browserCapabilities).toEqual({ allow_writes: false, can_edit_presentation: false });
    expect(api.browserSession?.session_id).toBe(sourceID); expect(api.projectId).toBe(projects[0].id);
    expect(api.browserCapabilities.can_edit_presentation).toBe(true);
    expect(f.cookies.has(sourceID)).toBe(true); // No source logout before the caller adopts the verified target.
    const calls = f.fetcher.mock.calls;
    expect(calls.map(([url]) => new URL(String(url)).pathname)).toEqual([
      '/api/v1/projects/project_one/browser-session/switch', '/api/v1/instance', '/api/v1/projects/project_two', '/api/v1/projects/project_two/browser-session', '/api/v1/projects/project_two/presentation',
    ]);
    expect(JSON.parse(String(calls[0][1]?.body))).toEqual({ project_id: projects[1].id, session_id: targetID });
    for (const [index, [url, init]] of calls.entries()) {
      const headers = new Headers(init?.headers);
      expect(init).toMatchObject({ credentials: 'same-origin', redirect: 'error', cache: 'no-store' });
      expect(headers.get('X-FC-Browser-Session')).toBe(index ? targetID : sourceID);
      expect(headers.get('X-FC-Instance')).toBe(instance);
      for (const name of ['Authorization', 'X-FC-Worker', 'Idempotency-Key', 'Origin']) expect(headers.has(name)).toBe(false); // The browser supplies Origin itself.
      expect(String(url)).not.toContain(targetID); expect(String(url)).not.toContain(sourceID);
    }
  });

  it('trusts resumed target permissions rather than the switch receipt or source capabilities', async () => {
    const f = fixture(); f.cookies.set(sourceID, session(sourceID, projects[0].id, false));
    const api = await source(); f.state.targetAdmin = true;
    f.state.response = (value, phase) => phase === 'switch' ? { ...value, allow_writes: false, can_edit_presentation: false } : value;
    const next = target(); await verifyProjectSwitch(api, next, targetID, new AbortController().signal);
    expect(next.browserCapabilities.can_edit_presentation).toBe(true);
    expect(api.browserCapabilities.allow_writes).toBe(false);
  });

  it('keeps source and other-tab cookies when target access disappears, and cleans only the selected target', async () => {
    const f = fixture(), api = await source(), next = target();
    const otherID = 'c'.repeat(32); f.cookies.set(otherID, session(otherID));
    f.state.response = (value, phase) => { if (phase === 'switch') f.cookies.delete(targetID); return value; };
    await expect(verifyProjectSwitch(api, next, targetID, new AbortController().signal)).rejects.toMatchObject({ code: 'unauthorized' });
    await next.endBrowserSession();
    expect([...f.cookies.keys()]).toEqual([sourceID, otherID]);
    expect(api.browserSession?.project_id).toBe(projects[0].id);
  });

  it('can retire only the source cookie after successful verification without disturbing target or another tab', async () => {
    const f = fixture(), api = await source(), next = target();
    const otherID = 'c'.repeat(32); f.cookies.set(otherID, session(otherID));
    await verifyProjectSwitch(api, next, targetID, new AbortController().signal);
    api.close(); await api.endBrowserSession();
    expect([...f.cookies.keys()]).toEqual([otherID, targetID]);
    await next.connect(); expect(next.browserSession?.session_id).toBe(targetID);
  });

  it('does not read the target after a definite refusal and keeps the source cookie usable', async () => {
    const f = fixture(), api = await source(); f.state.switchMode = 'denied'; f.fetcher.mockClear();
    await expect(verifyProjectSwitch(api, target(), targetID, new AbortController().signal)).rejects.toMatchObject({ code: 'forbidden', outcomeUnknown: false });
    expect(f.fetcher).toHaveBeenCalledTimes(1); expect(f.cookies.has(sourceID)).toBe(true);
    expect(api.browserSession?.session_id).toBe(sourceID);
  });

  it.each(['instance', 'project', 'selector', 'expired'])('refuses an unverified target %s while preserving the source', async kind => {
    const f = fixture(), api = await source(), next = target();
    f.state.response = (value, phase) => phase !== 'current' || value.project_id !== projects[1].id ? value : kind === 'instance' ? { ...value, instance_id: 'foreign_instance' } : kind === 'project' ? { ...value, project_id: projects[0].id } : kind === 'selector' ? { ...value, session_id: sourceID } : { ...value, expires_at: 'not-a-time' };
    await expect(verifyProjectSwitch(api, next, targetID, new AbortController().signal)).rejects.toMatchObject({ code: 'invalid_response' });
    expect(next.browserSession).toBeUndefined(); expect(api.browserSession?.session_id).toBe(sourceID); expect(f.cookies.has(sourceID)).toBe(true);
  });

  it('rejects handles, foreign IDs and invalid/reused selectors before dispatch', async () => {
    const f = fixture(), api = await source(); f.fetcher.mockClear();
    for (const [projectID, selector] of [['two', targetID], ['project_foreign', targetID], [projects[0].id, targetID], [projects[1].id, sourceID], [projects[1].id, 'B'.repeat(32)], [projects[1].id, 'c'.repeat(31)]]) await expect(api.switchBrowserSession(projectID, selector)).rejects.toMatchObject({ code: 'invalid_request' });
    expect(f.fetcher).not.toHaveBeenCalled();
  });

  it('bounds dispatch, refuses overlap and cancels late target verification without adopting it', async () => {
    const f = fixture(), api = await source();
    vi.useFakeTimers();
    let deliver!: (response: Response) => void;
    f.fetcher.mockImplementationOnce((_url, init) => new Promise<Response>((resolve, reject) => { deliver = resolve; init?.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError'))); }));
    const pending = api.switchBrowserSession(projects[1].id, targetID).catch(error => error);
    await expect(api.switchBrowserSession(projects[1].id, 'c'.repeat(32))).rejects.toMatchObject({ code: 'invalid_request' });
    await vi.advanceTimersByTimeAsync(5000);
    expect(await pending).toMatchObject({ code: 'deadline_exceeded', outcomeUnknown: true });
    deliver(json(session(targetID, projects[1].id)));
    expect(api.browserSession?.session_id).toBe(sourceID);
    const cancelled = new AbortController(); cancelled.abort(); f.fetcher.mockClear();
    await expect(verifyProjectSwitch(api, target(), targetID, cancelled.signal)).rejects.toMatchObject({ code: 'cancelled', outcomeUnknown: false });
    expect(f.fetcher).not.toHaveBeenCalled();
  });
});

describe('project selector and local safety', () => {
  it('renders an accessible literal native selector only for a signed-in multi-project catalog', () => {
    const props = { projectID: projects[0].id, projects, busy: false, select: vi.fn() };
    const tree = ProjectSwitcher(props)!;
    const html = renderToStaticMarkup(tree);
    expect(html).toContain('aria-label="Switch project"'); expect(html).toContain('value="project_one" selected=""');
    expect(html).toContain('One &lt;literal&gt;</option>'); expect(html).toContain('value="project_two"');
    expect(renderToStaticMarkup(<ProjectSwitcher {...props} activeName="Current name" />)).toContain('selected="">Current name</option>');
    expect(renderToStaticMarkup(<ProjectSwitcher {...props} projects={[projects[0], { ...projects[1], name: projects[0].name }]} />)).toContain('One &lt;literal&gt; · one');
    expect(html).not.toContain('disabled'); expect(html).not.toContain('credential');
    tree.props.children[1].props.onChange({ target: { value: projects[1].id } });
    expect(props.select).toHaveBeenCalledWith(projects[1].id);
    expect(renderToStaticMarkup(<ProjectSwitcher {...props} busy />)).toContain('disabled="" aria-busy="true"');
    for (const options of [undefined, [], [projects[0]], [projects[0], projects[0]], [projects[1], { id: 'foreign', handle: 'foreign', name: 'Foreign' }]]) expect(renderToStaticMarkup(<ProjectSwitcher {...props} projects={options} />)).toBe('');
  });

  it('blocks unresolved actions, collapsed dirty/busy drafts and focused draft editors but not read-only pagination', () => {
    let marker: string | undefined, editing = false;
    const root = { visibilityState: 'visible', activeElement: { matches: () => editing }, querySelector: (query: string) => marker && query.includes(marker) ? {} : null } as unknown as Document;
    expect(browserProjectSwitchSafe(0, root)).toBe(true);
    expect(browserProjectSwitchSafe(1, root)).toBe(false);
    for (marker of ['data-browser-dirty', 'data-browser-busy']) expect(browserProjectSwitchSafe(0, root)).toBe(false);
    marker = 'data-browser-window-unsafe'; expect(browserProjectSwitchSafe(0, root)).toBe(true);
    marker = undefined; editing = true; expect(browserProjectSwitchSafe(0, root)).toBe(false);
  });

  it('stores only the target public binding, forgets only the retired source and leaves another tab intact', () => {
    const tab = storage(), otherTab = storage(), recovery = storage();
    const source: BrowserSessionBinding = { version: 1, projectID: projects[0].id, projectHandle: projects[0].handle, instanceID: instance, sessionID: sourceID, expiresAt: session().expires_at, selectedworkerID: 'source_worker' };
    const other = { ...source, sessionID: 'c'.repeat(32) };
    const next = { ...source, projectID: projects[1].id, projectHandle: projects[1].handle, sessionID: targetID, selectedworkerID: '' };
    expect(saveBrowserSession(source, tab, recovery)).toBe(true);
    expect(saveBrowserSession(other, otherTab, recovery)).toBe(true);
    expect(saveBrowserSession(next, tab, recovery)).toBe(true);
    forgetBrowserSession(source, tab, recovery);
    expect(loadBrowserSession('two', undefined, tab, recovery)).toEqual(next);
    expect(loadBrowserSession('one', undefined, otherTab, recovery)).toEqual(other);
    expect(JSON.parse(recovery.values.get(browserRecoveryKey)!)).toEqual([other, next]);
    expect(Object.keys(JSON.parse(tab.values.get(browserSessionKey)!)).sort()).toEqual(Object.keys(next).sort());
    expect(tab.values.get(browserSessionKey)).not.toMatch(/source_worker|credential|projects|permissions|allow_writes/u);
  });
});
