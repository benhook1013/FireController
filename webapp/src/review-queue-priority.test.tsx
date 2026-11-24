import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Api } from './api';
import { BrowserActionMemory } from './browser-actions';
import { WorkspaceContext, type Session } from './workspace';
import { ReviewPullPriorityControl, refreshReviewQueuePriority, reviewQueuePriorityKey } from './review-scheduling';
import { ReviewPullFacts } from './review-workspace';
import type { ReviewPullSummary } from './types';

const project = { id: 'project_one', name: 'Project', github_repository_id: 1, repository: 'example/project' };
const settings = { project_id: project.id, revision: 3, priority_pulls: [7] };
const clients: Api[] = [];
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
async function connected(writes = true) {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => String(input).endsWith('/instance')
    ? json({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session')
      ? json({ session_id: JSON.parse(String(init?.body)).session_id, instance_id: 'instance_one', project_id: project.id, expires_at: '2099-01-01T00:00:00Z', allow_writes: writes, can_edit_presentation: false }) : json(project)));
  const api = new Api('PRIVATE_TOKEN', project.id);
  clients.push(api);
  await api.connect(); await api.startBrowserSession();
  return api;
}
afterEach(() => { clients.splice(0).forEach(api => api.close()); vi.unstubAllGlobals(); });

describe('queue priority browser contract', () => {
  it('reads default-empty settings and rejects malformed or cross-project settings', async () => {
    const api = await connected();
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      expect(new URL(String(input)).pathname).toBe('/api/v1/projects/project_one/reviews/queue-priority');
      return json({ ...settings, revision: 0, priority_pulls: [] });
    }));
    expect(await api.reviewQueuePriority()).toEqual({ ...settings, revision: 0, priority_pulls: [] });
    for (const value of [{ ...settings, project_id: 'other' }, { ...settings, revision: -1 }, { ...settings, revision: 1.5 }, { ...settings, priority_pulls: [0] }, { ...settings, priority_pulls: [7, 7] }, { ...settings, priority_pulls: ['7'] }]) {
      vi.stubGlobal('fetch', vi.fn(async () => json(value)));
      await expect(api.reviewQueuePriority()).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });

  it('requires sorted nonempty PRs, revision and key before dispatch', async () => {
    const api = await connected();
    const fetch = vi.fn(); vi.stubGlobal('fetch', fetch);
    for (const numbers of [[], [0], [-7], [7, 7], [8, 7], [1.5]]) await expect(api.setReviewQueuePriority(3, numbers, true, 'key')).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(api.setReviewQueuePriority(-1, [7], true, 'key')).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(api.setReviewQueuePriority(3, [7], true, '')).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetch).not.toHaveBeenCalled();
  });

  it('uses browser write authority and preserves exact body and key after an unknown result', async () => {
    const api = await connected();
    const bodies: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      bodies.push(String(init?.body));
      const headers = new Headers(init?.headers);
      expect(init?.method).toBe('POST'); expect(init?.credentials).toBe('same-origin');
      expect(headers.has('authorization')).toBe(false);
      expect(headers.has('x-fc-worker')).toBe(false);
      expect(headers.get('idempotency-key')).toBe('original-key');
      expect(JSON.parse(String(init?.body))).toEqual({ expected_revision: 3, pull_numbers: [7], priority: false });
      return bodies.length === 1 ? json({ ...settings, project_id: 'other' }) : json({ ...settings, revision: 4, priority_pulls: [] });
    }));
    const memory = new BrowserActionMemory(() => 'original-key');
    const input = { revision: 3, priority: false };
    const execute = (value: typeof input, key: string, signal: AbortSignal) => api.setReviewQueuePriority(value.revision, [7], value.priority, key, signal);
    const signal = new AbortController().signal;
    await expect(memory.run('priority-7', input, execute, signal)).rejects.toMatchObject({ outcomeUnknown: true });
    expect(bodies).toHaveLength(1);
    await memory.run('priority-7', { ...input, revision: 99, priority: true }, execute, signal);
    expect(bodies[1]).toBe(bodies[0]); expect(memory.unresolved).toBe(0);
  });

  it('rejects readonly writes locally and treats a revision conflict as a known refusal', async () => {
    const reader = await connected(false);
    const fetch = vi.fn(); vi.stubGlobal('fetch', fetch);
    await expect(reader.setReviewQueuePriority(0, [7], true, 'key')).rejects.toMatchObject({ status: 403, outcomeUnknown: false });
    expect(fetch).not.toHaveBeenCalled();
    const writer = await connected();
    vi.stubGlobal('fetch', vi.fn(async () => json({ error: { code: 'review_conflict', message: 'Changed' }, request_id: 'request_one' }, 409)));
    await expect(writer.setReviewQueuePriority(3, [7], true, 'key')).rejects.toMatchObject({ status: 409, outcomeUnknown: false });
  });
});

describe('compact priority presentation', () => {
  function fixture(writes = true, worker = true, loaded = true) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const key = ['workspace', project.id];
    const api = { browserCapabilities: { allow_writes: writes, can_edit_presentation: false }, reviewQueuePriority: vi.fn() };
    const session = { api, project, worker: worker ? { id: 'worker_one' } : null, controller: new AbortController(), actions: new BrowserActionMemory() } as unknown as Session;
    if (loaded) client.setQueryData(reviewQueuePriorityKey(key), settings);
    const render = (children: ReactNode) => renderToStaticMarkup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project_one', ready: true, fail: vi.fn(), selectWorker: vi.fn() }}>{children}</WorkspaceContext.Provider></QueryClientProvider>);
    return { client, key, api, render };
  }

  it('subscribes rows to shared settings without fetching and exposes a pressed toggle', () => {
    const f = fixture();
    try {
      const html = f.render(<><ReviewPullPriorityControl number={7} /><ReviewPullPriorityControl number={8} /></>);
      expect(html).toContain('aria-label="Priority for PR #7" aria-pressed="true"');
      expect(html).toContain('aria-label="Priority for PR #8" aria-pressed="false"');
      expect(html).not.toContain('disabled'); expect(f.api.reviewQueuePriority).not.toHaveBeenCalled();
    } finally { f.client.clear(); }
  });

  it.each([[false, true, true], [true, false, true], [true, true, false]])('uses write capability and loaded revision (%s,%s,%s)', (writes, worker, loaded) => {
    const f = fixture(writes, worker, loaded);
    try {
      const html = f.render(<ReviewPullPriorityControl number={7} />);
      if (!writes) expect(html).toBe(''); else if (!loaded) expect(html).toContain('disabled'); else expect(html).not.toContain('disabled');
      expect(f.api.reviewQueuePriority).not.toHaveBeenCalled();
    } finally { f.client.clear(); }
  });

  it('shows the metadata badge only for a direct row flag, never for front order', () => {
    const f = fixture();
    const pull: ReviewPullSummary = { number: 7, title: 'Change', state: 'open', head_sha: 'a'.repeat(40), base_sha: 'b'.repeat(40), base_ref: 'main', ci_state: 'unknown', mergeability: 'unknown', complete: true, run_count: 0, run_counts: { hosted: 0, cli: 0, manual: 0, subagent: 0 }, attempt_count: 0, recent_runs: [], latest_attempts: [], queue_position: 0 };
    try {
      expect(f.render(<ReviewPullFacts pull={pull} />)).not.toContain('review-priority-badge');
      expect(f.render(<ReviewPullFacts pull={{ ...pull, priority: true }} />)).toContain('review-priority-badge');
    } finally { f.client.clear(); }
  });

  it('invalidates the shared guard, rendered queue and fronts without touching another project', async () => {
    const f = fixture();
    const queue = [...f.key, 'reviews', 'current', 'queue', 'recent', ''];
    const fronts = [...f.key, 'reviews', 'current', 'fronts'];
    const other = ['workspace', 'other', 'reviews', 'current', 'fronts'];
    for (const key of [queue, fronts, other]) f.client.setQueryData(key, {});
    try {
      await refreshReviewQueuePriority(f.client, f.key);
      for (const key of [reviewQueuePriorityKey(f.key), queue, fronts]) expect(f.client.getQueryState(key)?.isInvalidated).toBe(true);
      expect(f.client.getQueryState(other)?.isInvalidated).toBe(false);
    } finally { f.client.clear(); }
  });
});
