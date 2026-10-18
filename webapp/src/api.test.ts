import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api, ApiError, validateMessageEventPage } from './api';

const project = { id: 'project_one', name: 'Test project', github_repository_id: 123, repository: 'owner/repository' };
const worker = { id: 'worker_one', project_id: 'project_one', name: 'Builder' };
const inboxItem = {
  id: 'msg_one', conversation_id: 'conv_one', sender_worker_id: 'worker_two', recipient_worker_id: 'worker_one',
  preview: 'hello', preview_truncated: false, seen: false, acknowledged: false, created_at: '2026-10-15T12:00:00Z',
};
const jobSummary = { id: 'job_one', title: 'Task', worker_id: 'worker_one', status: 'active', revision: 1 };
const job = {
  ...jobSummary, project_id: 'project_one', brief: 'private brief', created_at: '2026-10-15T12:00:00Z', updated_at: '2026-10-15T12:00:00Z',
};
const message = {
  id: 'msg_one', conversation_id: 'conv_one', sender_worker_id: 'worker_two', recipient_worker_id: 'worker_one',
  body: 'private body', created_at: '2026-10-15T12:00:00Z',
};
const checkpoint = {
  id: 'checkpoint_one', project_id: 'project_one', job_id: 'job_one', job_revision: 2, sequence: 9,
  actor_id: 'principal_one', worker_id: 'worker_one', done: '  Finished exactly\n🔥 �', next: 'next\tstep', blocker: '', evidence: ['second proof', 'first proof'], created_at: '2026-10-15T12:00:00Z',
};
const reviewSnapshot = {
  project_id: 'project_one', revision: 1, snapshot_id: `review_snapshot_${'a'.repeat(32)}`,
  source: 'retained test observations', observed_at: '2026-10-15T12:00:00Z', recorded_at: '2026-10-15T12:01:00Z',
};
const reviewRun = {
  source: 'original-review-producer',
  id: `review_run_${'b'.repeat(32)}`, pull_number: 1, source_run_id: 'run-source-one', channel: 'hosted',
  head_sha: 'c'.repeat(40), reviewer: 'Reviewer', scope: 'selected patch', coverage_limits: ['Historical selected scope only.'],
  outcome: 'completed', attributable: true, complete: true, started_at: '2026-10-15T11:00:00Z', finished_at: '2026-10-15T11:05:00Z',
  found: 1, accepted: 1, routed: 0, finalized: true, fixed: 1,
};
const reviewFinding = {
  source_finding_id: 'finding-one', title: '<Untrusted finding>', detail: 'Retained finding detail.', severity: 'medium',
  disposition: 'accepted', decision_reason: 'Reproduced locally.', actor: 'Builder', decided_at: '2026-10-15T11:10:00Z',
  fix_sha: 'd'.repeat(40), proof_note: 'Focused proof passed.', resolved_at: '2026-10-15T11:15:00Z',
};
const reviewAttempt = {
  source_attempt_id: 'attempt-one', channel: 'hosted', head_sha: reviewRun.head_sha, state: 'completed',
  started_at: reviewRun.started_at, finished_at: reviewRun.finished_at, duration_seconds: 300,
  source_run_id: reviewRun.source_run_id, trigger_id: '', provider_review_id: '',
};
const reviewPull = {
  number: 1, title: 'Retained merged work', state: 'merged', head_sha: reviewRun.head_sha, base_ref: 'main', base_sha: 'e'.repeat(40),
  ci_state: 'success', mergeability: 'unknown', complete: true, run_count: 1, attempt_count: 1,
  recent_runs: [reviewRun], latest_attempts: [reviewAttempt],
};
const reviewQueue = { ...reviewSnapshot, configured: true, items: [reviewPull], has_more: false };
const reviewDetail = { ...reviewSnapshot, pull_number: 1, run: reviewRun, findings: [reviewFinding] };

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
}
function setupLocation(): void { vi.stubGlobal('location', { origin: 'https://service.example' }); }
function api(): Api { return new Api('private-token-do-not-leak', 'project_one'); }
async function connect(client: Api): Promise<void> {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session') ? json({ session_id: 'a'.repeat(32), instance_id: 'instance_one', project_id: 'project_one', expires_at: new Date(Date.now() + 3600000).toISOString(), allow_writes: false, can_edit_presentation: false }) : json(project)));
  await client.connect();
}

afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('read-only browser cookie session transport', () => {
  it('exchanges verified bearer once, then uses only the selector cookie mode for finite reads and logout', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const requests: Array<{ url: string; init: RequestInit }> = [];
    let selector = '';
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push({ url: String(input), init: init! });
      if (init?.method === 'POST') {
        selector = (JSON.parse(String(init.body)) as { session_id: string }).session_id;
        expect(selector).toMatch(/^[a-f0-9]{32}$/);
        expect(JSON.parse(String(init.body))).toEqual({ session_id: selector });
        expect(new Headers(init.headers).get('authorization')).toBe('Bearer private-token-do-not-leak');
        expect(new Headers(init.headers).has('x-fc-browser-session')).toBe(false);
        return json({ session_id: selector, instance_id: 'instance_one', project_id: 'project_one', expires_at: new Date(Date.now() + 24 * 3600000).toISOString(), allow_writes: false, can_edit_presentation: false });
      }
      if (init?.method === 'DELETE') return json({ session_id: selector, logged_out: true });
      return json({ items: [worker], has_more: false });
    }));
    const started = await client.startBrowserSession();
    expect(started.session_id).toBe(selector);
    expect(JSON.stringify(client)).not.toContain('private-token-do-not-leak');
    expect((await client.workers()).items).toEqual([worker]);
    client.close();
    // Logout remains independent of the now-aborted read transport.
    await client.endBrowserSession();
    for (const [index, { url, init }] of requests.entries()) {
      expect(new URL(url).origin).toBe('https://service.example');
      expect(init.credentials).toBe('same-origin');
      expect(new Headers(init.headers).get('x-fc-instance')).toBe('instance_one');
      if (index > 0) {
        expect(new Headers(init.headers).get('authorization')).toBeNull();
        expect(new Headers(init.headers).get('x-fc-browser-session')).toBe(selector);
      }
      expect(url).not.toContain(selector);
    }
    expect(requests[2].init.body).toBe('{}');
  });

  it('resumes exact expected identity without bearer and sends cookie-mode headers for snapshot and SSE', async () => {
    setupLocation();
    const selector = 'a'.repeat(32);
    const client = Api.resume('project_one', selector, 'instance_one');
    const requests: RequestInit[] = [];
    const controller = new AbortController();
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(init!);
      const url = String(input);
      if (url.endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (url.endsWith('/project_one')) return json(project);
      if (url.endsWith('/browser-session')) return json({ session_id: selector, instance_id: 'instance_one', project_id: 'project_one', expires_at: new Date(Date.now() + 3600000).toISOString(), allow_writes: false, can_edit_presentation: false });
      if (url.includes('/snapshot')) return json({ inbox: { items: [], has_more: false }, event_cursor: 'snapshot_cursor' });
      const page = { items: [], cursor: 'stream_cursor', has_more: false, timed_out: false };
      const data = `event: message-events\nid: stream_cursor\ndata: ${JSON.stringify(page)}\n\n`;
      return new Response(new ReadableStream<Uint8Array>({ start(output) { output.enqueue(new TextEncoder().encode(data)); } }), { headers: { 'content-type': 'text/event-stream' } });
    }));
    await client.watchMessages('worker_one', {}, { snapshot() {}, events() { controller.abort(); }, status() {} }, controller.signal);
    expect(requests).toHaveLength(5);
    for (const init of requests) {
      expect(init.credentials).toBe('same-origin');
      expect(new Headers(init.headers).get('authorization')).toBeNull();
      expect(new Headers(init.headers).get('x-fc-browser-session')).toBe(selector);
      expect(new Headers(init.headers).get('x-fc-instance')).toBe('instance_one');
    }
    expect(new Headers(requests[4].headers).get('x-fc-worker')).toBe('worker_one');
    client.close();
  });

  it('rejects malformed restored bindings before HTTP and instance mismatch before project reads', async () => {
    setupLocation();
    const fetchMock = vi.fn().mockResolvedValue(json({ instance_id: 'instance_other' }));
    vi.stubGlobal('fetch', fetchMock);
    for (const args of [['../project', 'a'.repeat(32), 'instance_one'], ['project_one', 'A'.repeat(32), 'instance_one'], ['project_one', 'a'.repeat(32), ''], ['project_one', 'a'.repeat(33), 'instance_one']]) {
      expect(() => Api.resume(args[0], args[1], args[2])).toThrow(ApiError);
    }
    expect(fetchMock).not.toHaveBeenCalled();
    const client = Api.resume('project_one', 'a'.repeat(32), 'instance_one');
    await expect(client.connect()).rejects.toMatchObject({ code: 'instance_mismatch' });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(client.instanceId).toBeUndefined();
    client.close();
  });

  it('strictly rejects incomplete, unbound or oversized session receipts and supports safe cleanup after failure', async () => {
    setupLocation();
    for (const kind of ['missing', 'null', 'unknown', 'project', 'instance', 'selector', 'oversize']) {
      const client = api(); await connect(client);
      let selector = '';
      vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        if (init?.method === 'DELETE') return json({ session_id: selector, logged_out: true });
        selector = (JSON.parse(String(init?.body)) as { session_id: string }).session_id;
        const value: Record<string, unknown> = { session_id: selector, instance_id: 'instance_one', project_id: 'project_one', expires_at: new Date(Date.now() + 3600000).toISOString(), allow_writes: false, can_edit_presentation: false };
        if (kind === 'missing') delete value.expires_at;
        if (kind === 'null') value.expires_at = null;
        if (kind === 'unknown') value.token = 'private-token-do-not-leak';
        if (kind === 'project') value.project_id = 'project_two';
        if (kind === 'instance') value.instance_id = 'instance_other';
        if (kind === 'selector') value.session_id = 'b'.repeat(32);
        if (kind === 'oversize') return new Response('x'.repeat(4097), { headers: { 'content-type': 'application/json' } });
        return json(value);
      }));
      await expect(client.startBrowserSession()).rejects.toMatchObject({ code: kind === 'oversize' ? 'response_too_large' : 'invalid_response' });
      client.close(); await client.endBrowserSession();
    }
  });

  it('accepts a canonical server expiry even when it is past the device clock', async () => {
    setupLocation();
    const client = api(); await connect(client);
    let selector = '';
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === 'DELETE') return json({ session_id: selector, logged_out: true });
      selector = (JSON.parse(String(init?.body)) as { session_id: string }).session_id;
      return json({ session_id: selector, instance_id: 'instance_one', project_id: 'project_one', expires_at: '2000-01-01T00:00:00Z', allow_writes: false, can_edit_presentation: false });
    }));
    const started = await client.startBrowserSession();
    expect(started).toMatchObject({ expires_at: '2000-01-01T00:00:00Z', allow_writes: false, can_edit_presentation: false });
    expect(client.browserSession).toEqual(started);
    await client.endBrowserSession();
  });

  it('cancels an old restore and permits a fresh StrictMode-style instance to verify the retained binding', async () => {
    setupLocation();
    const old = Api.resume('project_one', 'a'.repeat(32), 'instance_one');
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
    })));
    const pending = old.connect();
    old.close();
    await expect(pending).rejects.toMatchObject({ code: 'cancelled' });
    const fresh = Api.resume('project_one', 'a'.repeat(32), 'instance_one');
    await connect(fresh);
    expect(fresh.instanceId).toBe('instance_one');
    fresh.close();
  });

  it('requires the exact bounded logout receipt instead of treating an arbitrary 200 as success', async () => {
    setupLocation();
    for (const value of [{}, { session_id: 'b'.repeat(32), logged_out: true }, { session_id: 'a'.repeat(32), logged_out: null }, { session_id: 'a'.repeat(32), logged_out: true, extra: 'private' }]) {
      const client = Api.resume('project_one', 'a'.repeat(32), 'instance_one');
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(value)));
      await expect(client.endBrowserSession()).rejects.toMatchObject({ code: 'invalid_response' });
      client.close();
    }
    const client = Api.resume('project_one', 'a'.repeat(32), 'instance_one');
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response('x'.repeat(4097), { headers: { 'content-type': 'application/json' } })));
    await expect(client.endBrowserSession()).rejects.toMatchObject({ code: 'response_too_large' });
    client.close();
  });
});

describe('Api stored review workspace', () => {
  it('requires a bounded original run source in compact and full reads independently of the publisher', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const { source: _producer, ...missingSource } = reviewRun;
    for (const kind of ['queue', 'pull', 'runs', 'detail']) {
      for (const run of [missingSource, ...[null, '', ' \t\n', '🔥'.repeat(33), 'bad\u0000source'].map(source => ({ ...reviewRun, source }))]) {
        const envelope = kind === 'queue' ? { ...reviewQueue, items: [{ ...reviewPull, recent_runs: [run] }] }
          : kind === 'pull' ? { ...reviewSnapshot, pull: { ...reviewPull, recent_runs: [run] }, attempts: [reviewAttempt] }
          : kind === 'runs' ? { ...reviewSnapshot, items: [run], has_more: false }
          : { ...reviewDetail, run };
        vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(envelope)));
        const request = kind === 'queue' ? client.reviewQueue() : kind === 'pull' ? client.reviewPull(1) : kind === 'runs' ? client.reviewRuns(1) : client.reviewRun(reviewRun.id);
        await expect(request).rejects.toMatchObject({ code: 'invalid_response' });
      }
    }
  });

  it('reads retained merged history and four source channels using scoped GETs and explicit snapshot pages', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const channels = ['cli', 'hosted', 'manual', 'subagent'];
    const attempts = channels.map((channel, index) => ({ ...reviewAttempt, channel, source_attempt_id: `attempt-${index}` }));
    const pull = { ...reviewPull, attempt_count: 4, latest_attempts: attempts };
    const runs = channels.map((channel, index) => ({ ...reviewRun, channel, id: `review_run_${String(index).repeat(32)}`, source_run_id: `run-${index}` }));
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(json({ ...reviewQueue, items: [pull], has_more: true, cursor: 'signed_queue_cursor' }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, pull, attempts }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, items: runs, has_more: true, cursor: 'signed_runs_cursor' }))
      .mockResolvedValueOnce(json(reviewDetail))
      .mockResolvedValueOnce(json({ project_id: 'project_one', configured: false, revision: 0, snapshot_id: '', source: '', observed_at: '', recorded_at: '', items: [], has_more: false }));
    vi.stubGlobal('fetch', fetchMock);
    expect((await client.reviewQueue({ limit: 1, cursor: 'previous_queue_cursor' })).items[0].state).toBe('merged');
    expect((await client.reviewPull(1, reviewSnapshot.snapshot_id)).attempts.map(item => item.channel)).toEqual(channels);
    expect((await client.reviewRuns(1, { snapshot: reviewSnapshot.snapshot_id, limit: 4, cursor: 'previous_runs_cursor' })).items.map(item => item.id)).toEqual(runs.map(item => item.id));
    const detail = await client.reviewRun(reviewRun.id, reviewSnapshot.snapshot_id);
    expect(detail.findings[0]).toEqual(reviewFinding);
    expect(detail.run.coverage_limits).toEqual(reviewRun.coverage_limits);
    expect(detail.run.source).toBe('original-review-producer');
    expect(detail.source).toBe(reviewSnapshot.source);
    expect((await client.reviewQueue()).configured).toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(5);
    const urls = fetchMock.mock.calls.map(call => new URL(call[0]));
    expect(urls[0].pathname).toBe('/api/v1/projects/project_one/reviews/queue');
    expect(urls[0].searchParams.get('limit')).toBe('1');
    expect(urls[0].searchParams.get('cursor')).toBe('previous_queue_cursor');
    expect(urls[1].pathname).toBe('/api/v1/projects/project_one/reviews/pulls/1');
    expect(urls[2].pathname).toBe('/api/v1/projects/project_one/reviews/pulls/1/runs');
    expect(urls[2].searchParams.get('cursor')).toBe('previous_runs_cursor');
    expect(urls[3].pathname).toBe(`/api/v1/projects/project_one/reviews/runs/${reviewRun.id}`);
    for (const [index, call] of fetchMock.mock.calls.entries()) {
      expect(urls[index].origin).toBe('https://service.example');
      expect(call[1].method ?? 'GET').toBe('GET');
      expect(new Headers(call[1].headers).get('x-fc-instance')).toBe('instance_one');
      expect(urls[index].href).not.toContain('private-token');
      if (index >= 1 && index <= 3) expect(urls[index].searchParams.get('snapshot')).toBe(reviewSnapshot.snapshot_id);
    }
  });

  it.each(['preparing', 'held', 'prepared'])('preserves the distinct %s attempt state in queue and pull reads', async (state) => {
    setupLocation();
    const client = api();
    await connect(client);
    const attempt = { ...reviewAttempt, state, finished_at: '', duration_seconds: null };
    const pull = { ...reviewPull, latest_attempts: [attempt] };
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(json({ ...reviewQueue, items: [pull] }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, pull, attempts: [attempt] })));
    expect((await client.reviewQueue()).items[0].latest_attempts[0].state).toBe(state);
    expect((await client.reviewPull(1)).attempts[0].state).toBe(state);
    client.close();
  });

  it('rejects malformed attempt states and keeps preparing/held out of run outcomes', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    for (const state of ['queued', 'HELD', 'preparing ', '', 1, null]) {
      const attempt = { ...reviewAttempt, state };
      const pull = { ...reviewPull, latest_attempts: [attempt] };
      vi.stubGlobal('fetch', vi.fn()
        .mockResolvedValueOnce(json({ ...reviewQueue, items: [pull] }))
        .mockResolvedValueOnce(json({ ...reviewSnapshot, pull, attempts: [attempt] })));
      await expect(client.reviewQueue()).rejects.toMatchObject({ code: 'invalid_response' });
      await expect(client.reviewPull(1)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    for (const outcome of ['preparing', 'held']) {
      const run = { ...reviewRun, outcome };
      const pull = { ...reviewPull, recent_runs: [run] };
      vi.stubGlobal('fetch', vi.fn()
        .mockResolvedValueOnce(json({ ...reviewQueue, items: [pull] }))
        .mockResolvedValueOnce(json({ ...reviewSnapshot, pull, attempts: [reviewAttempt] }))
        .mockResolvedValueOnce(json({ ...reviewSnapshot, items: [run], has_more: false }))
        .mockResolvedValueOnce(json({ ...reviewDetail, run })));
      await expect(client.reviewQueue()).rejects.toMatchObject({ code: 'invalid_response' });
      await expect(client.reviewPull(1)).rejects.toMatchObject({ code: 'invalid_response' });
      await expect(client.reviewRuns(1)).rejects.toMatchObject({ code: 'invalid_response' });
      await expect(client.reviewRun(run.id)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    client.close();
  });

  it('rejects uppercase and mixed-case review SHAs across public response views', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    for (const value of ['A'.repeat(40), `aB${'c'.repeat(38)}`]) {
      const cases: Array<{ name: string; body: unknown; read: () => Promise<unknown> }> = [
        { name: 'queue pull head', body: { ...reviewQueue, items: [{ ...reviewPull, head_sha: value }] }, read: () => client.reviewQueue() },
        { name: 'pull base', body: { ...reviewSnapshot, pull: { ...reviewPull, base_sha: value }, attempts: [reviewAttempt] }, read: () => client.reviewPull(1) },
        { name: 'queue run head', body: { ...reviewQueue, items: [{ ...reviewPull, recent_runs: [{ ...reviewRun, head_sha: value }] }] }, read: () => client.reviewQueue() },
        { name: 'run page head', body: { ...reviewSnapshot, items: [{ ...reviewRun, head_sha: value }], has_more: false }, read: () => client.reviewRuns(1) },
        { name: 'run detail head', body: { ...reviewDetail, run: { ...reviewRun, head_sha: value } }, read: () => client.reviewRun(reviewRun.id) },
        { name: 'queue attempt head', body: { ...reviewQueue, items: [{ ...reviewPull, latest_attempts: [{ ...reviewAttempt, head_sha: value }] }] }, read: () => client.reviewQueue() },
        { name: 'pull attempt head', body: { ...reviewSnapshot, pull: reviewPull, attempts: [{ ...reviewAttempt, head_sha: value }] }, read: () => client.reviewPull(1) },
        { name: 'finding fix', body: { ...reviewDetail, findings: [{ ...reviewFinding, fix_sha: value }] }, read: () => client.reviewRun(reviewRun.id) },
      ];
      for (const test of cases) {
        const fetchMock = vi.fn().mockResolvedValueOnce(json(test.body));
        vi.stubGlobal('fetch', fetchMock);
        await expect(test.read(), test.name).rejects.toMatchObject({ code: 'invalid_response' });
        expect(fetchMock, test.name).toHaveBeenCalledTimes(1);
        expect(fetchMock.mock.calls[0][1].method ?? 'GET', test.name).toBe('GET');
      }
    }
  });

  it('preserves empty unknown review SHAs without inventing identities', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const run = { ...reviewRun, head_sha: '', fixed: 0 };
    const attempt = { ...reviewAttempt, head_sha: '' };
    const pull = { ...reviewPull, head_sha: '', base_sha: '', recent_runs: [run], latest_attempts: [attempt] };
    const finding = { ...reviewFinding, fix_sha: '', proof_note: '', resolved_at: '' };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(json({ ...reviewQueue, items: [pull] }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, pull, attempts: [attempt] }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, items: [run], has_more: false }))
      .mockResolvedValueOnce(json({ ...reviewDetail, run, findings: [finding] }));
    vi.stubGlobal('fetch', fetchMock);
    expect((await client.reviewQueue()).items[0].head_sha).toBe('');
    expect((await client.reviewPull(1)).pull.base_sha).toBe('');
    expect((await client.reviewRuns(1)).items[0].head_sha).toBe('');
    expect((await client.reviewRun(run.id)).findings[0].fix_sha).toBe('');
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  it.each<Array<[string, Partial<typeof reviewRun>]>[number]>([
    ['completed missing start', { complete: false, started_at: '' }],
    ['completed missing finish', { complete: false, finished_at: '' }],
    ['completed missing both', { complete: false, started_at: '', finished_at: '' }],
    ['complete unknown missing start', { outcome: 'unknown', started_at: '' }],
    ['complete unknown missing finish', { outcome: 'unknown', finished_at: '' }],
    ['complete unknown missing both', { outcome: 'unknown', started_at: '', finished_at: '' }],
  ])('rejects %s across all stored review views', async (_name, patch) => {
    setupLocation();
    const client = api();
    await connect(client);
    const run = { ...reviewRun, ...patch };
    const pull = { ...reviewPull, recent_runs: [run] };
    const views: Array<[unknown, () => Promise<unknown>]> = [
      [{ ...reviewQueue, items: [pull] }, () => client.reviewQueue()],
      [{ ...reviewSnapshot, pull, attempts: [reviewAttempt] }, () => client.reviewPull(1)],
      [{ ...reviewSnapshot, items: [run], has_more: false }, () => client.reviewRuns(1)],
      [{ ...reviewDetail, run }, () => client.reviewRun(run.id)],
    ];
    for (const [body, read] of views) {
      const fetchMock = vi.fn().mockResolvedValueOnce(json(body));
      vi.stubGlobal('fetch', fetchMock);
      await expect(read()).rejects.toMatchObject({ code: 'invalid_response' });
      expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(fetchMock.mock.calls[0][1].method ?? 'GET').toBe('GET');
    }
  });

  it.each([
    ['start', '', reviewAttempt.finished_at],
    ['finish', reviewAttempt.started_at, ''],
    ['both', '', ''],
  ])('rejects completed attempts missing %s timestamps in queue and pull views', async (_name, start, finish) => {
    setupLocation();
    const client = api();
    await connect(client);
    const attempt = { ...reviewAttempt, started_at: start, finished_at: finish };
    const pull = { ...reviewPull, latest_attempts: [attempt] };
    for (const [body, read] of [
      [{ ...reviewQueue, items: [pull] }, () => client.reviewQueue()],
      [{ ...reviewSnapshot, pull, attempts: [attempt] }, () => client.reviewPull(1)],
    ] as Array<[unknown, () => Promise<unknown>]>) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(body)));
      await expect(read()).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });

  it.each(['runs', 'attempts'])('rejects positive %s counts with an empty corresponding summary', async kind => {
    setupLocation();
    const client = api();
    await connect(client);
    const pull = kind === 'runs' ? { ...reviewPull, recent_runs: [] } : { ...reviewPull, latest_attempts: [] };
    for (const [body, read] of [
      [{ ...reviewQueue, items: [pull] }, () => client.reviewQueue()],
      [{ ...reviewSnapshot, pull, attempts: [reviewAttempt] }, () => client.reviewPull(1)],
    ] as Array<[unknown, () => Promise<unknown>]>) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(body)));
      await expect(read()).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });

  it('rejects a resolved accepted finding without its effective fix SHA', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    // Keep the fixed count consistent so this isolates the missing SHA guard.
    const body = { ...reviewDetail, run: { ...reviewRun, fixed: 0 }, findings: [{ ...reviewFinding, fix_sha: '', proof_note: '' }] };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(body)));
    await expect(client.reviewRun(reviewRun.id)).rejects.toMatchObject({ code: 'invalid_response' });
  });

  it.each(['unknown', 'ordered complete', 'completed'])('retains legitimate %s history and unresolved accepted findings', async shape => {
    setupLocation();
    const client = api();
    await connect(client);
    const run = { ...reviewRun, fixed: 0, ...(shape === 'unknown' ? { complete: false, outcome: 'unknown', started_at: '', finished_at: '' } : shape === 'ordered complete' ? { outcome: 'unknown' } : {}) };
    const attempt = { ...reviewAttempt, ...(shape === 'unknown' ? { state: 'unknown', started_at: '', finished_at: '', duration_seconds: null } : {}) };
    const finding = { ...reviewFinding, fix_sha: '', proof_note: 'Accepted; fix evidence remains unknown.', resolved_at: '' };
    const pull = { ...reviewPull, recent_runs: [run], latest_attempts: [attempt] };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(json({ ...reviewQueue, items: [pull] }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, pull, attempts: [attempt] }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, items: [run], has_more: false }))
      .mockResolvedValueOnce(json({ ...reviewDetail, run, findings: [finding] }));
    vi.stubGlobal('fetch', fetchMock);
    expect((await client.reviewQueue()).items[0].recent_runs[0]).toEqual(run);
    expect((await client.reviewPull(1)).attempts[0]).toEqual(attempt);
    expect((await client.reviewRuns(1)).items[0]).toEqual(run);
    expect((await client.reviewRun(run.id)).findings[0]).toEqual(finding);
    expect(fetchMock).toHaveBeenCalledTimes(4);
    for (const call of fetchMock.mock.calls) expect(call[1].method ?? 'GET').toBe('GET');
  });

  it('retains genuinely empty zero-count review histories', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const pull = { ...reviewPull, run_count: 0, attempt_count: 0, recent_runs: [], latest_attempts: [] };
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(json({ ...reviewQueue, items: [pull] }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, pull, attempts: [] }))
      .mockResolvedValueOnce(json({ ...reviewSnapshot, items: [], has_more: false })));
    expect((await client.reviewQueue()).items[0]).toEqual(pull);
    expect((await client.reviewPull(1)).attempts).toEqual([]);
    expect((await client.reviewRuns(1)).items).toEqual([]);
  });

  it('rejects invalid review selectors and paging before any request', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    for (const call of [
      () => client.reviewQueue({ limit: 101 }), () => client.reviewQueue({ cursor: ' padded ' }),
      () => client.reviewPull(0), () => client.reviewPull(2147483648), () => client.reviewPull(1, ''),
      () => client.reviewRuns(1, { snapshot: '../other' }), () => client.reviewRuns(1, { limit: 0 }),
      () => client.reviewRun('run_other'), () => client.reviewRun(reviewRun.id, 'bad_snapshot'),
    ]) await expect(call()).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('requires complete scoped metadata and safe parsed review shapes', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const { project_id: _project, ...missingProject } = reviewQueue;
    const { coverage_limits: _limits, ...missingLimits } = reviewRun;
    const invalidQueues = [
      missingProject, { ...reviewQueue, project_id: 'project_two' }, { ...reviewQueue, snapshot_id: 'snapshot_bad' },
      { ...reviewQueue, observed_at: null }, { ...reviewQueue, extra: 'private-token-do-not-leak' },
      { ...reviewQueue, items: [{ ...reviewPull, state: { toString: null } }] },
      { ...reviewQueue, items: [{ ...reviewPull, recent_runs: [missingLimits] }] },
      { ...reviewQueue, items: [{ ...reviewPull, latest_attempts: [reviewAttempt, reviewAttempt], attempt_count: 2 }] },
      { ...reviewQueue, configured: false }, { ...reviewQueue, has_more: true },
      { ...reviewQueue, items: [reviewPull, reviewPull] },
    ];
    for (const value of invalidQueues) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(value)));
      const error = await client.reviewQueue().catch((failure: unknown) => failure);
      expect(error).toBeInstanceOf(ApiError);
      expect(error).toMatchObject({ code: 'invalid_response' });
      expect((error as Error).message).not.toContain('private-token');
    }
    for (const value of [
      { ...reviewSnapshot, pull: { ...reviewPull, number: 2, recent_runs: [] }, attempts: [reviewAttempt] },
      { ...reviewSnapshot, snapshot_id: `review_snapshot_${'f'.repeat(32)}`, pull: reviewPull, attempts: [reviewAttempt] },
      { ...reviewSnapshot, pull: reviewPull, attempts: [] },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(value)));
      await expect(client.reviewPull(1, reviewSnapshot.snapshot_id)).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });

  it('honors default and explicit page sizes, route identity and cursor completeness', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const pulls = Array.from({ length: 21 }, (_, index) => ({ ...reviewPull, number: index + 1, run_count: 0, recent_runs: [], attempt_count: 0, latest_attempts: [] }));
    const runs = Array.from({ length: 21 }, (_, index) => ({ ...reviewRun, id: `review_run_${index.toString(16).padStart(32, '0')}`, source_run_id: `run-${index}` }));
    for (const kind of ['queue', 'runs']) {
      const page = { ...reviewSnapshot, ...(kind === 'queue' ? { configured: true } : {}), items: kind === 'queue' ? pulls : runs, has_more: false };
      vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json(page))));
      await expect(kind === 'queue' ? client.reviewQueue() : client.reviewRuns(1)).rejects.toMatchObject({ code: 'invalid_response' });
      const explicit = kind === 'queue' ? await client.reviewQueue({ limit: 21 }) : await client.reviewRuns(1, { limit: 21 });
      expect(explicit.items).toHaveLength(21);
    }
    for (const page of [
      { ...reviewSnapshot, items: [{ ...reviewRun, pull_number: 2 }], has_more: false },
      { ...reviewSnapshot, items: [reviewRun, reviewRun], has_more: false },
      { ...reviewSnapshot, items: [reviewRun], has_more: true },
      { ...reviewSnapshot, items: [], has_more: true, cursor: 'cursor' },
      { ...reviewSnapshot, items: [reviewRun], has_more: false, cursor: 'cursor' },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(page)));
      await expect(client.reviewRuns(1)).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });

  it('validates full finding completeness, attribution fields and bounded source limitations', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const { findings: _findings, ...missingFindings } = reviewDetail;
    for (const value of [
      missingFindings, { ...reviewDetail, project_id: 'project_two' }, { ...reviewDetail, pull_number: 2 },
      { ...reviewDetail, run: { ...reviewRun, id: `review_run_${'0'.repeat(32)}` } },
      { ...reviewDetail, findings: [] }, { ...reviewDetail, findings: [reviewFinding, reviewFinding], run: { ...reviewRun, found: 2, accepted: 2, fixed: 2 } },
      { ...reviewDetail, findings: [{ ...reviewFinding, disposition: 'rejected' }] },
      { ...reviewDetail, findings: [{ ...reviewFinding, detail: 'x'.repeat((16 << 10) + 1) }] },
      { ...reviewDetail, findings: [{ ...reviewFinding, title: '\ud800' }] },
      { ...reviewDetail, findings: [{ ...reviewFinding, proof_note: 'bad\u0000proof' }] },
      { ...reviewDetail, run: { ...reviewRun, fixed: 0 } },
      { ...reviewDetail, run: { ...reviewRun, accepted: 2 } },
      { ...reviewDetail, run: { ...reviewRun, coverage_limits: ['x'.repeat(1025)] } },
      { ...reviewDetail, run: { ...reviewRun, coverage_limits: Array(17).fill('bounded') } },
      { ...reviewDetail, run: { ...reviewRun, started_at: reviewRun.finished_at, finished_at: reviewRun.started_at } },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(value)));
      await expect(client.reviewRun(reviewRun.id)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    const incomplete = { ...reviewDetail, findings: [], run: { ...reviewRun, channel: 'subagent', complete: false, finalized: false, attributable: false, fixed: 0, outcome: 'unknown', started_at: '', finished_at: '' } };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(incomplete)));
    expect((await client.reviewRun(reviewRun.id)).run.complete).toBe(false);
  });

  it('retains unknown attempt duration without converting it into measured zero', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const attempt = { ...reviewAttempt, duration_seconds: null };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ ...reviewSnapshot, pull: { ...reviewPull, latest_attempts: [attempt] }, attempts: [attempt] })));
    expect((await client.reviewPull(1)).attempts[0].duration_seconds).toBeNull();
    for (const duration_seconds of [-1, '0', Number.MAX_SAFE_INTEGER + 1]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ ...reviewSnapshot, pull: reviewPull, attempts: [{ ...reviewAttempt, duration_seconds }] })));
      await expect(client.reviewPull(1)).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });

  it('uses the separate four MiB run-detail budget and cancels review requests', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const findings = Array.from({ length: 100 }, (_, index) => ({ ...reviewFinding, source_finding_id: `finding-${index}`, detail: 'x'.repeat(12 << 10) }));
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ ...reviewDetail, findings, run: { ...reviewRun, found: 100, accepted: 100, fixed: 100 } })));
    expect((await client.reviewRun(reviewRun.id)).findings).toHaveLength(100);
    for (const [size, call] of [[1 << 20, () => client.reviewQueue()], [4 << 20, () => client.reviewRun(reviewRun.id)]] as const) {
      const cancelled = vi.fn();
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(new ReadableStream<Uint8Array>({
        start(controller) { controller.enqueue(new Uint8Array(size + 1)); }, cancel: cancelled,
      }), { headers: { 'content-type': 'application/json' } })));
      await expect(call()).rejects.toMatchObject({ code: 'response_too_large' });
      expect(cancelled).toHaveBeenCalledTimes(1);
    }
    const controller = new AbortController();
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
    })));
    const pending = client.reviewRuns(1, {}, controller.signal);
    controller.abort();
    await expect(pending).rejects.toMatchObject({ code: 'cancelled' });
    const detail = client.reviewRun(reviewRun.id);
    client.close();
    await expect(detail).rejects.toMatchObject({ code: 'cancelled' });
  });
});

describe('Api binding and bounded reads', () => {
  it('verifies instance then project and binds every read to same origin, instance and selected worker', async () => {
    setupLocation();
    const client = api();
    const calls: Array<{ url: string; init: RequestInit }> = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init: init ?? {} });
      if (url.endsWith('/api/v1/instance')) return json({ instance_id: 'instance_one' });
      if (url.endsWith('/api/v1/projects/project_one')) return json(project);
      if (url.endsWith('/workers') || url.includes('/workers?')) return json({ items: [worker], has_more: false, cursor: '' });
      if (url.includes('/jobs?')) return json({ items: [jobSummary], has_more: false, cursor: '' });
      if (url.endsWith('/jobs/job_one')) return json(job);
      if (url.includes('/inbox/worker_one/snapshot')) return json({ inbox: { items: [inboxItem], has_more: false, cursor: '' }, event_cursor: 'signed_event_cursor' });
      if (url.includes('/inbox/worker_one?') || url.endsWith('/inbox/worker_one')) return json({ items: [inboxItem], has_more: false, cursor: '' });
      if (url.includes('/conversations/conv_one?') || url.endsWith('/conversations/conv_one')) return json({ conversation_id: 'conv_one', root_message_id: 'msg_one', items: [message], has_more: false, cursor: '' });
      throw new Error('unexpected request');
    }));

    const verified = await client.connect();
    expect(verified.id).toBe('project_one');
    expect(client.instanceId).toBe('instance_one');
    expect((await client.workers()).items).toEqual([worker]);
    expect((await client.jobs({ status: 'active', worker_id: 'worker_one', limit: 10 })).items).toEqual([jobSummary]);
    expect((await client.job('job_one')).brief).toBe('private brief');
    expect((await client.inbox('worker_one', { unread: true, unacknowledged: true })).items).toEqual([inboxItem]);
    expect((await client.snapshot('worker_one')).event_cursor).toBe('signed_event_cursor');
    expect((await client.thread('conv_one')).items).toEqual([message]);

    expect(calls).toHaveLength(8);
    expect(calls[0].url).toBe('https://service.example/api/v1/instance');
    expect(new Headers(calls[0].init.headers).get('authorization')).toBe('Bearer private-token-do-not-leak');
    expect(new Headers(calls[0].init.headers).get('x-fc-instance')).toBeNull();
    for (const call of calls.slice(1)) {
      const url = new URL(call.url);
      expect(url.origin).toBe('https://service.example');
      expect(url.search).not.toContain('private-token');
      expect(new Headers(call.init.headers).get('x-fc-instance')).toBe('instance_one');
      expect(call.init.credentials).toBe('omit');
      expect(call.init.redirect).toBe('error');
      expect(call.init.cache).toBe('no-store');
    }
    expect(new Headers(calls[5].init.headers).get('x-fc-worker')).toBe('worker_one');
    expect(calls[5].url).toContain('unread=true');
    expect(calls[5].url).toContain('unacknowledged=true');
    expect(new Headers(calls[6].init.headers).get('x-fc-worker')).toBe('worker_one');
  });

  it('preserves valid Unicode and control content in job titles, briefs, message bodies, previews and snapshots', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const content = `ANSI \u001b[31mred\u001b[0m <script>literal</script> café 🔥 ${Array.from({ length: 32 }, (_, index) => String.fromCharCode(index)).join('')}\u007f`;
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(json({ ...job, title: content, brief: content }))
      .mockResolvedValueOnce(json({ items: [{ ...jobSummary, title: content }], has_more: false }))
      .mockResolvedValueOnce(json({ conversation_id: 'conv_one', root_message_id: 'msg_one', items: [{ ...message, body: content }], has_more: false }))
      .mockResolvedValueOnce(json({ items: [{ ...inboxItem, preview: content }], has_more: false }))
      .mockResolvedValueOnce(json({ inbox: { items: [{ ...inboxItem, preview: content }], has_more: false }, event_cursor: 'signed_event_cursor' })));
    const detail = await client.job('job_one');
    expect(detail.title).toBe(content);
    expect(detail.brief).toBe(content);
    expect((await client.jobs()).items[0].title).toBe(content);
    expect((await client.thread('conv_one')).items[0].body).toBe(content);
    expect((await client.inbox('worker_one')).items[0].preview).toBe(content);
    expect((await client.snapshot('worker_one')).inbox.items[0].preview).toBe(content);
    client.close();
  });

  it.each([
    { field: 'title', label: 'UTF-8 byte boundary', value: 'é'.repeat(120), valid: true },
    { field: 'title', label: 'UTF-8 byte overflow', value: 'é'.repeat(121), valid: false },
    { field: 'title', label: 'empty content', value: '', valid: false },
    { field: 'title', label: 'wrong type', value: null, valid: false },
    { field: 'title', label: 'unpaired surrogate', value: '\ud800', valid: false },
    { field: 'brief', label: 'UTF-8 byte boundary', value: 'é'.repeat(16384), valid: true },
    { field: 'brief', label: 'UTF-8 byte overflow', value: 'é'.repeat(16385), valid: false },
    { field: 'brief', label: 'empty content', value: '', valid: true },
    { field: 'brief', label: 'wrong type', value: 1, valid: false },
    { field: 'brief', label: 'unpaired surrogate', value: '\ud800', valid: false },
    { field: 'body', label: 'UTF-8 byte boundary', value: 'é'.repeat(8192), valid: true },
    { field: 'body', label: 'UTF-8 byte overflow', value: 'é'.repeat(8193), valid: false },
    { field: 'body', label: 'empty content', value: '', valid: false },
    { field: 'body', label: 'wrong type', value: null, valid: false },
    { field: 'body', label: 'unpaired surrogate', value: '\udfff', valid: false },
    { field: 'preview', label: 'Unicode rune boundary', value: '🔥'.repeat(160), valid: true },
    { field: 'preview', label: 'Unicode rune overflow', value: '🔥'.repeat(161), valid: false },
    { field: 'preview', label: 'empty content', value: '', valid: true },
    { field: 'preview', label: 'wrong type', value: [], valid: false },
    { field: 'preview', label: 'unpaired surrogate', value: '\ud800', valid: false },
  ])('keeps $field validation for $label', async ({ field, value, valid }) => {
    setupLocation();
    const client = api();
    await connect(client);
    const payload = field === 'brief' || field === 'title' ? { ...job, [field]: value }
      : field === 'body' ? { conversation_id: 'conv_one', root_message_id: 'msg_one', items: [{ ...message, body: value }], has_more: false }
        : { items: [{ ...inboxItem, preview: value }], has_more: false };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(payload)));
    const pending = field === 'brief' || field === 'title' ? client.job('job_one') : field === 'body' ? client.thread('conv_one') : client.inbox('worker_one');
    if (valid) await expect(pending).resolves.toEqual(payload);
    else await expect(pending).rejects.toMatchObject({ code: 'invalid_response' });
    client.close();
  });

  it('continues rejecting control content in credentials, identifiers, cursors and unrelated response fields', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    for (const control of ['\u0000', '\u001b', '\u007f']) {
      expect(() => new Api(`token${control}value`, 'project_one')).toThrowError(ApiError);
      expect(() => new Api('private-token', `project${control}one`)).toThrowError(ApiError);
      await expect(client.job(`job${control}one`)).rejects.toMatchObject({ code: 'invalid_request' });
      await expect(client.inbox('worker_one', { cursor: `cursor${control}one` })).rejects.toMatchObject({ code: 'invalid_request' });
    }
    expect(fetchMock).not.toHaveBeenCalled();
    fetchMock.mockResolvedValueOnce(json({ items: [{ ...worker, id: 'worker\u001bone' }], has_more: false }));
    await expect(client.workers()).rejects.toMatchObject({ code: 'invalid_response' });
    fetchMock.mockResolvedValueOnce(json({ ...job, worker_id: 'worker\u007fone' }));
    await expect(client.job('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    fetchMock.mockResolvedValueOnce(json({ items: [{ ...inboxItem, id: 'msg\u001bone' }], has_more: false }));
    await expect(client.inbox('worker_one')).rejects.toMatchObject({ code: 'invalid_response' });
    client.close();
  });

  it.each(['project', 'worker'])('matches server display-name bounds and Unicode whitespace rules for $0 names', async (kind) => {
    setupLocation();
    for (const [name, valid] of [
      ['Name\u001b[31mred\u001b[0m\u0000\u007f', true],
      ['é'.repeat(100), true],
      ['é'.repeat(101), false],
      ['', false],
      [123, false],
      ['\ud800', false],
      [' name', false],
      ['name\n', false],
      ['\u0085name', false],
      ['name\u2003', false],
      ['\ufeffname', true],
    ] as const) {
      const client = api();
      let pending: Promise<unknown>;
      if (kind === 'project') {
        vi.stubGlobal('fetch', vi.fn()
          .mockResolvedValueOnce(json({ instance_id: 'instance_one' }))
          .mockResolvedValueOnce(json({ ...project, name })));
        pending = client.connect();
      } else {
        await connect(client);
        vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ items: [{ ...worker, name }], has_more: false })));
        pending = client.workers();
      }
      if (valid) await expect(pending).resolves.toBeDefined();
      else await expect(pending).rejects.toMatchObject({ code: 'invalid_response' });
      client.close();
    }
  });

  it('rejects changed identity, incomplete pages and unexpected fields without exposing response text', async () => {
    setupLocation();
    const client = api();
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(json({ instance_id: 'instance_one' }))
      .mockResolvedValueOnce(json({ ...project, id: 'project_two' })));
    await expect(client.connect()).rejects.toMatchObject({ code: 'invalid_response' });
    expect(client.instanceId).toBeUndefined();

    await connect(client);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ items: [worker], has_more: false, cursor: '', secret_field: 'hidden' })));
    await expect(client.workers()).rejects.toMatchObject({ code: 'invalid_response' });

    const token = 'private-token-do-not-leak';
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ error: { code: 'forbidden', message: `token=${token} https://private.invalid/path` } }, 403)));
    const failure = await client.workers().catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(ApiError);
    expect(failure).toMatchObject({ code: 'forbidden', status: 403 });
    expect((failure as Error).message).not.toContain(token);
    expect((failure as Error).message).not.toContain('https://');

    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ error: { code: 'private-token-do-not-leak', message: 'hidden detail' } }, 500)));
    const unknownCode = await client.workers().catch((error: unknown) => error);
    expect(unknownCode).toMatchObject({ code: 'service_error', status: 500 });
    expect((unknownCode as Error).message).not.toContain('private-token-do-not-leak');
  });

  it('rejects oversized response bodies and prevents invalid arguments from making requests', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn().mockResolvedValueOnce(new Response(new ReadableStream<Uint8Array>({
      start(controller) { controller.enqueue(new Uint8Array((1 << 20) + 1)); controller.close(); },
    }), { status: 200, headers: { 'content-type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    await expect(client.workers()).rejects.toMatchObject({ code: 'response_too_large' });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    fetchMock.mockClear();
    await expect(client.jobs({ limit: 101 })).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(client.inbox('../other')).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(client.job('bad/id')).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shares the five-second connection budget across instance and project verification', async () => {
    setupLocation();
    vi.useFakeTimers();
    const client = api();
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      if (String(_input).endsWith('/api/v1/instance')) return Promise.resolve(json({ instance_id: 'instance_one' }));
      return new Promise<Response>((_resolve, reject) => init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true }));
    }));
    const assertion = expect(client.connect()).rejects.toMatchObject({ code: 'deadline_exceeded' });
    await vi.advanceTimersByTimeAsync(5000);
    await assertion;
    expect(client.instanceId).toBeUndefined();
  });

  it('cancels finite requests and rejects redirect failures without exposing transport text', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const controller = new AbortController();
    controller.abort();
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      if (init?.signal?.aborted) reject(new DOMException('aborted', 'AbortError'));
      else init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
    })));
    await expect(client.workers({}, controller.signal)).rejects.toMatchObject({ code: 'cancelled' });

    const redirect = vi.fn().mockRejectedValue(new TypeError('redirect to https://elsewhere.invalid with private-token-do-not-leak'));
    vi.stubGlobal('fetch', redirect);
    const failure = await client.workers().catch((error: unknown) => error);
    expect(failure).toMatchObject({ code: 'connection_failed' });
    expect((failure as Error).message).not.toContain('elsewhere.invalid');
    expect((failure as Error).message).not.toContain('private-token-do-not-leak');
    expect(redirect.mock.calls[0][1]).toMatchObject({ redirect: 'error' });
  });

  it('close cancels session-owned requests and prevents later requests', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
    }));
    vi.stubGlobal('fetch', fetchMock);
    const pending = client.workers();
    await Promise.resolve();
    client.close();
    await expect(pending).rejects.toMatchObject({ code: 'cancelled' });
    await expect(client.jobs()).rejects.toMatchObject({ code: 'cancelled' });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(client.instanceId).toBeUndefined();
  });

  it('validates event scope and rejects bodies or unknown payload fields', () => {
    const base = {
      items: [{ sequence: 1, project_id: 'project_one', type: 'message.sent', resource_id: 'msg_one', revision: 1, actor_id: 'principal_one', payload: { conversation_id: 'conv_one', recipient_worker_id: 'worker_one', sender_worker_id: 'worker_two' }, created_at: '2026-10-15T12:00:00Z' }],
      cursor: 'signed_cursor', has_more: false, timed_out: false,
    };
    expect(validateMessageEventPage(base, 'project_one', 'worker_one', 20)).toBe(true);
    expect(validateMessageEventPage(base, 'project_two', 'worker_one', 20)).toBe(false);
    expect(validateMessageEventPage(base, 'project_one', 'worker_two', 20)).toBe(false);
    expect(validateMessageEventPage({ ...base, items: [{ ...base.items[0], payload: { ...base.items[0].payload, body: 'private' } }] }, 'project_one', 'worker_one', 20)).toBe(false);
    expect(validateMessageEventPage({ ...base, private_extra: true }, 'project_one', 'worker_one', 20)).toBe(false);
  });

  it('uses authenticated SSE after a scoped snapshot and aborts without JSON polling', async () => {
    setupLocation();
    const client = api();
    const calls: Array<{ url: string; init: RequestInit }> = [];
    const page = { items: [], cursor: 'stream_cursor', has_more: false, timed_out: false };
    const data = `event: message-events\nid: stream_cursor\ndata: ${JSON.stringify(page)}\n\n`;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init: init ?? {} });
      if (url.endsWith('/api/v1/instance')) return json({ instance_id: 'instance_one' });
      if (url.endsWith('/api/v1/projects/project_one')) return json(project);
      if (url.includes('/inbox/worker_one/snapshot')) return json({ inbox: { items: [], cursor: '', has_more: false }, event_cursor: 'snapshot_cursor' });
      if (url.includes('/message-events/stream')) return new Response(new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode(data)); } }), { status: 200, headers: { 'content-type': 'text/event-stream' } });
      throw new Error('unexpected request');
    }));
    const abort = new AbortController();
    let eventCount = 0;
    const watch = client.watchMessages('worker_one', { unread: true }, {
      snapshot(value) { expect(value.event_cursor).toBe('snapshot_cursor'); },
      events(value) { eventCount += 1; expect(value.cursor).toBe('stream_cursor'); abort.abort(); },
      status() {},
    }, abort.signal);
    await watch;
    expect(eventCount).toBe(1);
    expect(calls).toHaveLength(4);
    const stream = calls[3];
    const url = new URL(stream.url);
    expect(url.origin).toBe('https://service.example');
    expect(url.pathname).toBe('/api/v1/projects/project_one/message-events/stream');
    expect(url.searchParams.get('cursor')).toBe('snapshot_cursor');
    expect(url.searchParams.get('limit')).toBe('20');
    expect(url.search).not.toContain('private-token');
    expect(new Headers(stream.init.headers).get('authorization')).toBe('Bearer private-token-do-not-leak');
    expect(new Headers(stream.init.headers).get('x-fc-instance')).toBe('instance_one');
    expect(new Headers(stream.init.headers).get('x-fc-worker')).toBe('worker_one');
    expect(stream.init.credentials).toBe('omit');
    expect(stream.init.redirect).toBe('error');
    expect(calls.every((call) => !call.url.includes('github.com'))).toBe(true);
  });

  it('does not start a watch when its caller signal is already cancelled', async () => {
    setupLocation();
    const client = api();
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    const controller = new AbortController();
    controller.abort();
    await client.watchMessages('worker_one', {}, { snapshot() {}, events() {}, status() {} }, controller.signal);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('explicit checkpoint reads', () => {
  it('preserves scoped latest/current revisions, explicit null and newest-first archive pages without extra reads', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const requests: Array<{ url: string; init?: RequestInit }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      requests.push({ url, init });
      if (url.endsWith('/latest')) return json({ job_id: 'job_one', job_revision: 3, checkpoint });
      return json({ items: [{ ...checkpoint, worker_id: '', evidence: [] }], has_more: true, cursor: 'older_cursor' });
    });
    vi.stubGlobal('fetch', fetchMock);
    const latest = await client.checkpointLatest('job_one');
    expect(latest.job_revision).toBe(3);
    expect(latest.checkpoint).toEqual(checkpoint);
    expect(latest.checkpoint?.job_revision).toBe(2);
    const page = await client.checkpoints('job_one', { limit: 1, cursor: 'explicit_cursor' });
    expect(page).toMatchObject({ has_more: true, cursor: 'older_cursor' });
    expect(page.items[0].worker_id).toBe('');
    expect(page.items[0].evidence).toEqual([]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(new URL(requests[0].url).pathname).toBe('/api/v1/projects/project_one/jobs/job_one/checkpoints/latest');
    expect(new URL(requests[0].url).search).toBe('');
    expect(new URL(requests[1].url).searchParams.get('limit')).toBe('1');
    expect(new URL(requests[1].url).searchParams.get('cursor')).toBe('explicit_cursor');
    for (const request of requests) {
      expect(request.init).toMatchObject({ method: 'GET', credentials: 'omit', redirect: 'error', cache: 'no-store' });
      expect(new Headers(request.init?.headers).get('x-fc-instance')).toBe('instance_one');
      expect(new Headers(request.init?.headers).get('authorization')).toBe('Bearer private-token-do-not-leak');
      expect(request.url).not.toContain('private-token');
    }
    fetchMock.mockResolvedValueOnce(json({ job_id: 'job_one', job_revision: 3, checkpoint: null }));
    expect((await client.checkpointLatest('job_one')).checkpoint).toBeNull();
  });

  it('rejects incomplete, malformed, unscoped or invalid checkpoint latest records', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const { evidence: _evidence, ...withoutEvidence } = checkpoint;
    const valid = { job_id: 'job_one', job_revision: 3, checkpoint };
    const invalid = [
      { job_id: 'job_one', job_revision: 3 }, { ...valid, job_id: null }, { ...valid, job_revision: null },
      { ...valid, checkpoint: [] }, { ...valid, checkpoint: { ...checkpoint, evidence: null } },
      { ...valid, checkpoint: withoutEvidence }, { ...valid, checkpoint: { ...checkpoint, private_extra: 'not declared' } },
      { ...valid, private_extra: true }, { ...valid, job_id: 'job_other' },
      { ...valid, checkpoint: { ...checkpoint, project_id: 'project_other' } },
      { ...valid, checkpoint: { ...checkpoint, job_id: 'job_other' } },
      { ...valid, checkpoint: { ...checkpoint, actor_id: '' } }, { ...valid, checkpoint: { ...checkpoint, worker_id: null } },
      { ...valid, checkpoint: { ...checkpoint, job_revision: 4 } }, { ...valid, checkpoint: { ...checkpoint, job_revision: 0 } },
      { ...valid, checkpoint: { ...checkpoint, sequence: 0 } }, { ...valid, checkpoint: { ...checkpoint, sequence: 1.5 } },
      { ...valid, checkpoint: { ...checkpoint, sequence: Number.MAX_SAFE_INTEGER + 1 } },
      { ...valid, checkpoint: { ...checkpoint, created_at: 'not a timestamp' } },
      { ...valid, checkpoint: { ...checkpoint, done: ' \n\t' } },
      { ...valid, checkpoint: { ...checkpoint, done: '\u0085' } },
      { ...valid, checkpoint: { ...checkpoint, done: 'bad\u0001' } },
      { ...valid, checkpoint: { ...checkpoint, next: 'bad\u007f' } },
      { ...valid, checkpoint: { ...checkpoint, done: 'bad\ud800' } },
      { ...valid, checkpoint: { ...checkpoint, next: 'bad\udc00' } },
      { ...valid, checkpoint: { ...checkpoint, done: 'é'.repeat(4097) } },
      { ...valid, checkpoint: { ...checkpoint, blocker: 'b'.repeat(4097) } },
      { ...valid, checkpoint: { ...checkpoint, evidence: Array(11).fill('reference') } },
      { ...valid, checkpoint: { ...checkpoint, evidence: [''] } },
      { ...valid, checkpoint: { ...checkpoint, evidence: ['e'.repeat(1025)] } },
      { ...valid, checkpoint: { ...checkpoint, done: 'd'.repeat(8192), next: 'n'.repeat(8192), blocker: 'b', evidence: [] } },
    ];
    for (const response of invalid) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(response)));
      await expect(client.checkpointLatest('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    }
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response('{malformed', { headers: { 'content-type': 'application/json' } })));
    await expect(client.checkpointLatest('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
  });

  it('requires complete descending archive pages and validates arguments before requests', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const older = { ...checkpoint, id: 'checkpoint_older', sequence: 8 };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ items: [checkpoint, older], has_more: false })));
    expect((await client.checkpoints('job_one')).items.map(item => item.sequence)).toEqual([9, 8]);
    for (const response of [
      { items: [older, checkpoint], has_more: false }, { items: [checkpoint, checkpoint], has_more: false },
      { items: null, has_more: false }, { items: [checkpoint] }, { items: [], has_more: true, cursor: 'next' },
      { items: [checkpoint], has_more: true }, { items: [checkpoint], has_more: false, cursor: 'unexpected' },
      { items: [checkpoint, older], has_more: false, unknown: true },
      { items: [{ ...checkpoint, project_id: 'project_other' }], has_more: false },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(response)));
      await expect(client.checkpoints('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    }
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ items: [checkpoint, older], has_more: false })));
    await expect(client.checkpoints('job_one', { limit: 1 })).rejects.toMatchObject({ code: 'invalid_response' });
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    await expect(client.checkpointLatest('../other')).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(client.checkpoints('job_one', { limit: 101 })).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(client.checkpoints('job_one', { cursor: '' })).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('enforces the default checkpoint page size while accepting an explicit larger limit', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const page = { items: Array.from({ length: 21 }, (_, index) => ({ ...checkpoint, id: `checkpoint_${index}`, sequence: 21 - index })), has_more: false };
    const fetchMock = vi.fn().mockImplementation(async () => json(page));
    vi.stubGlobal('fetch', fetchMock);
    await expect(client.checkpoints('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    expect((await client.checkpoints('job_one', { limit: 21 })).items).toHaveLength(21);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(new URL(String(fetchMock.mock.calls[0][0])).searchParams.has('limit')).toBe(false);
    expect(new URL(String(fetchMock.mock.calls[1][0])).searchParams.get('limit')).toBe('21');
  });

  it('bounds latest to 128 KiB and archive to 4 MiB without truncation or automatic paging', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const full = { ...checkpoint, done: '<'.repeat(8192), next: 'n'.repeat(8192), evidence: [] };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ job_id: 'job_one', job_revision: 2, checkpoint: full })));
    expect((await client.checkpointLatest('job_one')).checkpoint?.done).toHaveLength(8192);
    const archive = { items: Array.from({ length: 100 }, (_, index) => ({ ...full, id: `checkpoint_${index}`, sequence: 100 - index })), has_more: false };
    expect(new TextEncoder().encode(JSON.stringify(archive)).byteLength).toBeGreaterThan(1 << 20);
    const fetchMock = vi.fn().mockResolvedValueOnce(json(archive));
    vi.stubGlobal('fetch', fetchMock);
    expect((await client.checkpoints('job_one', { limit: 100 })).items).toHaveLength(100);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    for (const [kind, size] of [['latest', 128 << 10], ['archive', 4 << 20]] as const) {
      const cancelled = vi.fn();
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(new ReadableStream<Uint8Array>({
        start(controller) { controller.enqueue(new Uint8Array(size + 1)); }, cancel: cancelled,
      }), { headers: { 'content-type': 'application/json' } })));
      const call = kind === 'latest' ? client.checkpointLatest('job_one') : client.checkpoints('job_one');
      await expect(call).rejects.toMatchObject({ code: 'response_too_large' });
      expect(cancelled).toHaveBeenCalledTimes(1);
    }
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response('{}', { headers: { 'content-type': 'application/json', 'content-length': String((128 << 10) + 1) } })));
    await expect(client.checkpointLatest('job_one')).rejects.toMatchObject({ code: 'response_too_large' });
  });

  it('cancels checkpoint reads through their caller and session signals', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
    }));
    vi.stubGlobal('fetch', fetchMock);
    const controller = new AbortController();
    const latest = client.checkpointLatest('job_one', controller.signal);
    controller.abort();
    await expect(latest).rejects.toMatchObject({ code: 'cancelled' });
    const history = client.checkpoints('job_one');
    client.close();
    await expect(history).rejects.toMatchObject({ code: 'cancelled' });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});

describe('captured comparison policy assessment', () => {
  const evidenceID = `review_evidence_${'a'.repeat(32)}`;
  const assessment = {
    project_id: project.id, evidence_id: evidenceID, source: 'captured_comparison', policy_revision: 2,
    evaluated_at: '2026-10-15T12:01:00Z', state: 'held',
    anchor: { repository_id: 123, repository: 'owner/repository', pull_number: 7, parent_pull_number: 0,
      candidate_sha: 'b'.repeat(40), base_sha: 'c'.repeat(40), parent_sha: 'd'.repeat(40), merge_base_sha: 'e'.repeat(40),
      patch_sha256: 'f'.repeat(64), base_ref: 'main', observed_at: '2026-10-15T12:00:00Z' },
    channels: [{ channel: 'hosted', covered: true, pending: true }, { channel: 'cli', covered: false, pending: false }],
  };
  it('makes one scoped finite GET, preserves pending dominance, and returns no findings', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn().mockResolvedValueOnce(json(assessment));
    vi.stubGlobal('fetch', fetchMock);
    await expect(client.reviewPolicyAssessment(evidenceID)).resolves.toEqual(assessment);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe(`https://service.example/api/v1/projects/project_one/reviews/evidence/${evidenceID}/policy`);
    expect(init.method).toBe('GET');
    expect(init.cache).toBe('no-store');
    expect(init.redirect).toBe('error');
    expect(new Headers(init.headers).get('x-fc-instance')).toBe('instance_one');
    expect(new Headers(init.headers).has('x-fc-worker')).toBe(false);
    client.close();
  });
  it('accepts sufficient, unconfigured and legitimate pending results without vacuous sufficiency', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    for (const value of [
      { ...assessment, state: 'sufficient', channels: [{ channel: 'cli', covered: true, pending: false }] },
      { ...assessment, state: 'unconfigured', policy_revision: 0, channels: [] },
      { ...assessment, channels: [{ channel: 'hosted', covered: false, pending: true }] },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(value)));
      await expect(client.reviewPolicyAssessment(evidenceID)).resolves.toEqual(value);
    }
    client.close();
  });
  it('refuses malformed identifiers before connection state or HTTP', async () => {
    setupLocation();
    const client = api();
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    for (const value of ['', '../policy', evidenceID.toUpperCase(), ` ${evidenceID}`, `${evidenceID}\n`, `review_run_${'a'.repeat(32)}`]) {
      await expect(client.reviewPolicyAssessment(value)).rejects.toMatchObject({ code: 'invalid_request' });
    }
    expect(fetchMock).not.toHaveBeenCalled();
    client.close();
  });
  it('rejects missing/null fields, cross-scope data, incoherent requirements and invalid complete anchors', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const invalid: unknown[] = [
      { ...assessment, project_id: 'other_project' }, { ...assessment, evidence_id: `review_evidence_${'b'.repeat(32)}` },
      { ...assessment, source: 'current_github' }, { ...assessment, PolicyRevision: 2 }, { ...assessment, extra: true },
      { ...assessment, policy_revision: 0 }, { ...assessment, policy_revision: 1.5 }, { ...assessment, policy_revision: Number.MAX_SAFE_INTEGER + 1 },
      { ...assessment, state: 'sufficient' }, { ...assessment, state: 'merge_ready' }, { ...assessment, channels: [] },
      { ...assessment, state: 'unconfigured', channels: [], policy_revision: 1 },
      { ...assessment, state: 'held', channels: [{ channel: 'cli', covered: true, pending: false }] },
      { ...assessment, channels: [assessment.channels[0], assessment.channels[0]] },
      { ...assessment, channels: [{ channel: 'manual', covered: false, pending: false }] },
      { ...assessment, channels: [{ channel: 'cli', covered: 1, pending: false }] },
      { ...assessment, channels: [{ channel: 'cli', covered: false, pending: false, extra: false }] },
      { ...assessment, evaluated_at: '2026-02-31T12:00:00Z' }, { ...assessment, evaluated_at: '0001-01-01T00:00:00Z' },
    ];
    for (const field of Object.keys(assessment)) {
      const missing = { ...assessment } as Record<string, unknown>; delete missing[field];
      invalid.push(missing, { ...assessment, [field]: null });
    }
    for (const field of Object.keys(assessment.anchor)) {
      const missing = { ...assessment.anchor } as Record<string, unknown>; delete missing[field];
      invalid.push({ ...assessment, anchor: missing }, { ...assessment, anchor: { ...assessment.anchor, [field]: null } });
    }
    for (const field of ['channel', 'covered', 'pending']) {
      const missing = { ...assessment.channels[0] } as Record<string, unknown>; delete missing[field];
      invalid.push({ ...assessment, channels: [missing] }, { ...assessment, channels: [{ ...assessment.channels[0], [field]: null }] });
    }
    for (const changes of [
      { repository_id: 0 }, { repository: '../repo' }, { repository: 'owner/repo/extra' }, { pull_number: 2147483648 },
      { parent_pull_number: -1 }, { candidate_sha: '' }, { base_sha: 'A'.repeat(40) }, { parent_sha: 'x'.repeat(40) },
      { merge_base_sha: 'e'.repeat(39) }, { patch_sha256: 'f'.repeat(63) }, { base_ref: ' ' },
      { base_ref: 'main\u0085' }, { base_ref: '\ud800' }, { observed_at: 'invalid' }, { extra: true },
    ]) invalid.push({ ...assessment, anchor: { ...assessment.anchor, ...changes } });
    for (const value of invalid) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(value)));
      await expect(client.reviewPolicyAssessment(evidenceID)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    client.close();
  });
  it('rejects duplicate and escaped-alias keys in original bytes, invalid UTF-8 and trailing JSON', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const raw = JSON.stringify(assessment);
    for (const body of [
      raw.replace('"state":"held"', '"state":"sufficient","state":"held"'),
      raw.replace('"policy_revision":2', '"policy_revision":1,"policy\\u005frevision":2'),
      raw.replace('"parent_pull_number":0', '"parent_pull_number":1,"parent_pull_number":0'),
      raw.replace('"pending":true', '"pending":false,"pending":true'),
      raw.replace('"policy_revision":2', '"policy_revision":2.0'),
      raw.replace('"pull_number":7', '"pull_number":7e0'),
      `${raw}{}`, new Uint8Array([0xff]),
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(body, { headers: { 'content-type': 'application/json' } })));
      await expect(client.reviewPolicyAssessment(evidenceID)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    client.close();
  });
  it('enforces the actual 16KiB byte budget and cancels oversized streams', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const raw = JSON.stringify(assessment);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(raw.padEnd(16 << 10), { headers: { 'content-type': 'application/json' } })));
    await expect(client.reviewPolicyAssessment(evidenceID)).resolves.toEqual(assessment);
    const cancelled = vi.fn();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(new ReadableStream<Uint8Array>({
      start(controller) { controller.enqueue(new TextEncoder().encode(raw.padEnd((16 << 10) + 1))); }, cancel: cancelled,
    }), { headers: { 'content-type': 'application/json' } })));
    await expect(client.reviewPolicyAssessment(evidenceID)).rejects.toMatchObject({ code: 'response_too_large' });
    expect(cancelled).toHaveBeenCalledTimes(1);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(raw, { headers: { 'content-type': 'application/json', 'content-length': String((16 << 10) + 1) } })));
    await expect(client.reviewPolicyAssessment(evidenceID)).rejects.toMatchObject({ code: 'response_too_large' });
    client.close();
  });
  it.each(['review_held', 'review_capacity'])('preserves %s assessment refusals without provider diagnostics', async code => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn().mockResolvedValueOnce(json({ error: { code, message: 'PRIVATE_PROVIDER_DIAGNOSTIC <script>', details: { reason: 'repository_changed', private: 'PRIVATE_CAPTURE' } } }, 409));
    vi.stubGlobal('fetch', fetchMock);
    const failure = await client.reviewPolicyAssessment(evidenceID).catch(error => error as ApiError);
    expect(failure).toMatchObject({ code, status: 409 });
    expect(String(failure)).not.toMatch(/PRIVATE_|script|repository_changed/);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    client.close();
  });
  it.each([[401, 'unauthorized'], [403, 'forbidden'], [500, 'storage_unavailable']] as const)('keeps assessment error %s/%s scoped and safe', async (status, code) => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn().mockResolvedValueOnce(json({ error: { code, message: 'PRIVATE_DIAGNOSTIC' } }, status));
    vi.stubGlobal('fetch', fetchMock);
    const failure = await client.reviewPolicyAssessment(evidenceID).catch(error => error as ApiError);
    expect(failure).toMatchObject({ code, status });
    expect(String(failure)).not.toContain('PRIVATE_DIAGNOSTIC');
    expect(fetchMock).toHaveBeenCalledTimes(1);
    client.close();
  });
  it('obeys caller/session cancellation and one finite five-second deadline without retry', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    vi.useFakeTimers();
    const fetchMock = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init!.signal!.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
    }));
    vi.stubGlobal('fetch', fetchMock);
    const controller = new AbortController();
    const caller = expect(client.reviewPolicyAssessment(evidenceID, controller.signal)).rejects.toMatchObject({ code: 'cancelled' });
    controller.abort(); await caller;
    const deadline = expect(client.reviewPolicyAssessment(evidenceID)).rejects.toMatchObject({ code: 'deadline_exceeded' });
    await vi.advanceTimersByTimeAsync(5000); await deadline;
    const session = expect(client.reviewPolicyAssessment(evidenceID)).rejects.toMatchObject({ code: 'cancelled' });
    client.close(); await session;
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });
});
