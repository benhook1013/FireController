import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
const snapshot = { project_id: 'project_one', revision: 3, snapshot_id: `review_snapshot_${'a'.repeat(32)}`, source: 'native-executor', observed_at: '2026-10-17T12:00:00Z', recorded_at: '2026-10-20T03:00:00Z' };
const pull = (number: number, position: number, state = 'merged') => ({ number, queue_position: position, state, title: `Change ${number}`, head_sha: 'a'.repeat(40), base_ref: 'main', base_sha: 'b'.repeat(40), ci_state: 'success', mergeability: 'unknown', complete: true, run_count: 0, run_counts: { hosted: 0, cli: 0, subagent: 0, manual: 0 }, attempt_count: 0, recent_runs: [], latest_attempts: [] });
const page = (items: unknown[], head_position?: number) => ({ ...snapshot, configured: true, items, has_more: false, ...(head_position === undefined ? {} : { head_position }) });
const response = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } });
async function connected() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? response({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session') ? response({ session_id: 'a'.repeat(32), instance_id: 'instance_one', project_id: 'project_one', expires_at: new Date(Date.now() + 3600000).toISOString(), allow_writes: false, can_edit_presentation: false }) : response({ id: 'project_one', name: 'Project', github_repository_id: 123, repository: 'owner/repository' })));
  const api = Api.resume('project_one', 'a'.repeat(32), 'instance_one'); await api.connect(); return api;
}
afterEach(() => vi.unstubAllGlobals());
describe('bounded recent and merged queue reads', () => {
  it('selects local views and preserves original positions and continuation', async () => {
    const api = await connected();
    const recent = page([pull(16, 15), pull(19, 18, 'open')], 18);
    const merged = { ...page([pull(18, 17), pull(16, 15)], 18), has_more: true, cursor: 'signed_history_cursor' };
    const fetch = vi.fn().mockResolvedValueOnce(response(recent)).mockResolvedValueOnce(response(merged)); vi.stubGlobal('fetch', fetch);
    expect(await api.reviewQueue({ view: 'recent', limit: 2 })).toEqual(recent);
    expect(await api.reviewQueue({ view: 'merged', limit: 2, cursor: 'prior_history_cursor' })).toEqual(merged);
    expect(fetch.mock.calls.map(call => String(call[0]))).toEqual(['https://service.example/api/v1/projects/project_one/reviews/queue?limit=2&view=recent&include_observation=true', 'https://service.example/api/v1/projects/project_one/reviews/queue?limit=2&cursor=prior_history_cursor&view=merged&include_observation=true']); api.close();
  });
  it.each([
    ['recent', page([pull(2, 2), pull(1, 1)])], ['merged', page([pull(1, 1), pull(2, 2)])],
    ['recent', page([pull(1, 1), pull(2, 1)])], ['merged', page([pull(1, 1, 'open')])],
    ['recent', page([{ ...pull(1, 1), queue_position: undefined }])], ['recent', page([{ ...pull(1, 1), queue_position: -1 }])],
    ['recent', page([pull(1, 1)], -1)], ['recent', { ...page([pull(1, 1)]), head_position: null }],
  ] as const)('refuses malformed or contradictory %s view metadata', async (view, body) => {
    const api = await connected(); const fetch = vi.fn().mockResolvedValueOnce(response(body)); vi.stubGlobal('fetch', fetch);
    await expect(api.reviewQueue({ view })).rejects.toMatchObject({ code: 'invalid_response' }); expect(fetch).toHaveBeenCalledTimes(1); api.close();
  });
  it('does not accept filtered context on the unchanged all-queue contract', async () => {
    const api = await connected(); vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(response(page([pull(1, 1)], 2))));
    await expect(api.reviewQueue()).rejects.toMatchObject({ code: 'invalid_response' }); api.close();
  });
  it('refuses an unsupported selector before HTTP', async () => {
    const api = await connected(); const fetch = vi.fn(); vi.stubGlobal('fetch', fetch);
    await expect(api.reviewQueue({ view: 'all' as 'recent' })).rejects.toMatchObject({ code: 'invalid_request' }); expect(fetch).not.toHaveBeenCalled(); api.close();
  });
});
