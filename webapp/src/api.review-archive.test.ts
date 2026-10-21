import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';

const project = { id: 'project_one', name: 'Archive project', github_repository_id: 1, repository: 'owner/project' };
const snapshot = `review_snapshot_${'a'.repeat(32)}`;
const metadata = { project_id: project.id, revision: 1, snapshot_id: snapshot, source: 'archive-publisher', observed_at: '2026-10-18T00:00:00Z', recorded_at: '2026-10-18T00:00:01Z' };
const attempt = { source: 'original-producer-a', source_attempt_id: 'local-attempt', channel: 'hosted', head_sha: 'a'.repeat(40), state: 'completed', started_at: '2026-10-17T23:00:00Z', finished_at: '2026-10-17T23:01:00Z', duration_seconds: 60, source_run_id: 'source-run', trigger_id: '', provider_review_id: '' };
const { source: _source, ...compactAttempt } = attempt;
const run = { source: attempt.source, id: `review_run_${'b'.repeat(32)}`, pull_number: 123, source_run_id: 'source-run', channel: 'hosted', head_sha: attempt.head_sha, reviewer: 'Reviewer', scope: 'selected patch', coverage_limits: [], outcome: 'completed', attributable: true, complete: true, started_at: attempt.started_at, finished_at: attempt.finished_at, found: 0, accepted: 0, routed: 0, finalized: true, fixed: 0 };
const pull = { number: 123, title: 'Large retained history', state: 'merged', head_sha: attempt.head_sha, base_ref: 'main', base_sha: 'c'.repeat(40), ci_state: 'unknown', mergeability: 'unknown', complete: true, run_count: 137, run_counts: { hosted: 137, cli: 0, subagent: 0, manual: 0 }, attempt_count: 137, recent_runs: [run], latest_attempts: [compactAttempt] };
const clients: Api[] = [];
function json(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } }); }
async function connected() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = new Api('private-archive-token', project.id); clients.push(api);
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : json(project)));
  await api.connect(); return api;
}
afterEach(() => { for (const api of clients.splice(0)) api.close(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('bounded review archive API', () => {
  it('retains optional safe attempt reasons in compact and archive reads without adding requests', async () => {
    const api = await connected();
    for (const reason of [undefined, 'hosted_finished_inventory_unproven', 'hosted_allowance_cooldown', 'allowance_exhausted', 'another_safe_code_2', 'a'.repeat(128)]) {
      const item = { ...attempt, state: 'uncertain', ...(reason === undefined ? {} : { reason }) };
      const { source: _producer, ...compact } = item;
      const fetchMock = vi.fn().mockResolvedValueOnce(json({ ...metadata, items: [item], has_more: false })).mockResolvedValueOnce(json({ ...metadata, pull: { ...pull, latest_attempts: [compact] } }));
      vi.stubGlobal('fetch', fetchMock);
      const archived = (await api.reviewAttempts(123, { snapshot })).items[0];
      const latest = (await api.reviewPull(123, snapshot)).pull.latest_attempts[0];
      expect(archived).toEqual(item); expect(latest).toEqual(compact);
      expect(Object.hasOwn(archived, 'reason')).toBe(reason !== undefined);
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(fetchMock.mock.calls.map(([url]) => new URL(String(url)).pathname)).toEqual(['/api/v1/projects/project_one/reviews/pulls/123/attempts', '/api/v1/projects/project_one/reviews/pulls/123']);
      for (const [url, options] of fetchMock.mock.calls) { expect(new URL(String(url)).searchParams.get('snapshot')).toBe(snapshot); expect(options.method).toBe('GET'); }
    }
  });
  it('rejects malformed or private attempt reasons in both compact and archive responses', async () => {
    const api = await connected();
    for (const reason of [null, '', 1, false, [], {}, 'Uppercase', 'bad-code', 'two words', 'line\nbreak', '_prefix', 'non_ascii_é', 'a'.repeat(129)]) {
      const item = { ...attempt, state: 'held', reason };
      const { source: _producer, ...compact } = item;
      vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, items: [item], has_more: false })));
      await expect(api.reviewAttempts(123, { snapshot })).rejects.toMatchObject({ code: 'invalid_response' });
      vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, pull: { ...pull, latest_attempts: [compact] } })));
      await expect(api.reviewPull(123, snapshot)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, items: [{ ...attempt, state: 'held', reason: 'safe_code', error_body: 'PRIVATE_SENTINEL' }], has_more: false })));
    await expect(api.reviewAttempts(123, { snapshot })).rejects.toMatchObject({ code: 'invalid_response' });
  });
  it('permits omitted reasons in every state but refuses reasons outside diagnostic states', async () => {
    const api = await connected();
    for (const state of ['active', 'preparing', 'prepared', 'posting', 'completed', 'cancelled', 'unknown']) {
      const item = { ...attempt, state };
      vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, items: [item], has_more: false })));
      expect((await api.reviewAttempts(123, { snapshot })).items[0]).toEqual(item);
      const withReason = { ...item, reason: 'hosted_finished_inventory_unproven' };
      const { source: _producer, ...compact } = withReason;
      vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, items: [withReason], has_more: false })));
      await expect(api.reviewAttempts(123, { snapshot })).rejects.toMatchObject({ code: 'invalid_response' });
      vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, pull: { ...pull, latest_attempts: [compact] } })));
      await expect(api.reviewPull(123, snapshot)).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });
  it('accepts safe lifetime totals beyond100 but requires compact PR detail without its old attempts field', async () => {
    const api = await connected();
    for (const count of [137, Number.MAX_SAFE_INTEGER]) {
      vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, pull: { ...pull, run_count: count, run_counts: { hosted: count, cli: 0, subagent: 0, manual: 0 }, attempt_count: count } })));
      expect((await api.reviewPull(123, snapshot)).pull.attempt_count).toBe(count);
    }
    for (const value of [{ ...metadata, pull, attempts: [] }, ...[-1, 1.5, Number.MAX_SAFE_INTEGER + 1].map(count => ({ ...metadata, pull: { ...pull, attempt_count: count } }))]) {
      vi.stubGlobal('fetch', vi.fn(async () => json(value))); await expect(api.reviewPull(123, snapshot)).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });

  it('reads exactly one bounded source-qualified page per invocation, with explicit snapshot continuation', async () => {
    const api = await connected();
    const first = { ...metadata, items: [attempt, { ...attempt, source: 'original-producer-b' }], has_more: true, cursor: 'signed_attempts_2' };
    const last = { ...metadata, items: [{ ...attempt, source_attempt_id: 'tail' }], has_more: false };
    const fetchMock = vi.fn().mockResolvedValueOnce(json(first)).mockResolvedValueOnce(json(last)); vi.stubGlobal('fetch', fetchMock);
    expect((await api.reviewAttempts(123, { snapshot, limit: 2 })).items.map(item => item.source)).toEqual(['original-producer-a', 'original-producer-b']); expect(fetchMock).toHaveBeenCalledTimes(1);
    expect((await api.reviewAttempts(123, { snapshot, limit: 2, cursor: first.cursor })).items[0].source_attempt_id).toBe('tail'); expect(fetchMock).toHaveBeenCalledTimes(2);
    for (const [index, call] of fetchMock.mock.calls.entries()) {
      const url = new URL(String(call[0])); expect(url.pathname).toBe('/api/v1/projects/project_one/reviews/pulls/123/attempts'); expect(url.searchParams.get('snapshot')).toBe(snapshot); expect(url.searchParams.get('limit')).toBe('2'); expect(url.searchParams.get('cursor')).toBe(index === 0 ? null : first.cursor); expect(call[1].method).toBe('GET');
    }
  });

  it('bounds each page to20 by default and100 only when explicitly selected', async () => {
    const api = await connected(); const items = Array.from({ length: 21 }, (_, index) => ({ ...attempt, source_attempt_id: `attempt_${index}` }));
    vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, items, has_more: false })));
    await expect(api.reviewAttempts(123)).rejects.toMatchObject({ code: 'invalid_response' });
    expect((await api.reviewAttempts(123, { limit: 100 })).items).toHaveLength(21);
  });

  it('rejects invalid local archive selectors before any network access', async () => {
    const api = await connected(); const fetchMock = vi.fn(); vi.stubGlobal('fetch', fetchMock);
    for (const invoke of [() => api.reviewAttempts(0), () => api.reviewAttempts(2147483648), () => api.reviewAttempts(123, { limit: 0 }), () => api.reviewAttempts(123, { limit: 101 }), () => api.reviewAttempts(123, { snapshot: 'other_snapshot' }), () => api.reviewAttempts(123, { cursor: ' padded ' })]) await expect(invoke()).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('rejects malformed source, authority metadata, identities, times, fields and continuation shape', async () => {
    const api = await connected(); const page = { ...metadata, items: [attempt], has_more: false };
    const { source: _missing, ...missingSource } = attempt;
    const badItems = [missingSource, ...['', 'bad\u0000source', 'a'.repeat(129)].map(source => ({ ...attempt, source })), { ...attempt, source_attempt_id: '' }, { ...attempt, state: 'made_up' }, { ...attempt, duration_seconds: -1 }, { ...attempt, finished_at: '' }, { ...attempt, head_sha: 'not-a-sha' }, { ...attempt, secret: 'PRIVATE_SENTINEL' }];
    for (const value of [...badItems.map(item => ({ ...page, items: [item] })), { ...page, items: [attempt, attempt] }, { ...page, project_id: 'project_other' }, { ...page, snapshot_id: `review_snapshot_${'f'.repeat(32)}` }, { ...page, revision: 0 }, { ...page, source: '' }, { ...page, has_more: true }, { ...page, cursor: 'unexpected_tail_cursor' }, { ...page, items: [], has_more: true, cursor: 'cursor' }]) {
      vi.stubGlobal('fetch', vi.fn(async () => json(value))); await expect(api.reviewAttempts(123, { snapshot })).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });

  it('keeps the shared five-second read bound and cancellation without an extra request', async () => {
    const api = await connected(); vi.useFakeTimers();
    const fetchMock = vi.fn((_input: RequestInfo | URL, init: RequestInit) => new Promise<Response>((_resolve, reject) => { init.signal!.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true }); })); vi.stubGlobal('fetch', fetchMock);
    const pending = api.reviewAttempts(123, { snapshot }).catch(error => error); await vi.advanceTimersByTimeAsync(5000); expect(await pending).toMatchObject({ code: 'deadline_exceeded' }); expect(fetchMock).toHaveBeenCalledTimes(1);
    const selected = new AbortController(); const cancelled = api.reviewAttempts(123, { snapshot }, selected.signal).catch(error => error); selected.abort(); expect(await cancelled).toMatchObject({ code: 'cancelled' }); expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
