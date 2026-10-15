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
  vi.stubGlobal('fetch', vi.fn()
    .mockResolvedValueOnce(json({ instance_id: 'instance_one' }))
    .mockResolvedValueOnce(json(project)));
  await client.connect();
}

afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('Api stored review workspace', () => {
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
