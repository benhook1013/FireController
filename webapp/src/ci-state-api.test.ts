import { afterEach, expect, it, vi } from 'vitest';
import { Api } from './api';

const response = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } });
afterEach(() => vi.unstubAllGlobals());

it('retains not_required separately from unknown and rejects unsupported CI states', async () => {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? response({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session') ? response({ session_id: 'a'.repeat(32), instance_id: 'instance_one', project_id: 'project_one', expires_at: new Date(Date.now() + 3600000).toISOString(), allow_writes: false, can_edit_presentation: false }) : response({ id: 'project_one', name: 'Project', github_repository_id: 123, repository: 'owner/repository' })));
  const api = Api.resume('project_one', 'a'.repeat(32), 'instance_one');
  await api.connect();
  const pull = { number: 1, title: 'Selected PR', state: 'open', head_sha: 'a'.repeat(40), base_ref: 'main', base_sha: 'b'.repeat(40), ci_state: 'not_required', mergeability: 'unknown', complete: false, run_count: 0, run_counts: { hosted: 0, cli: 0, subagent: 0, manual: 0 }, attempt_count: 0, recent_runs: [], latest_attempts: [] };
  for (const ci_state of ['not_required', 'unknown', 'optional', 'NOT_REQUIRED']) {
    const page = { project_id: 'project_one', revision: 1, snapshot_id: `review_snapshot_${'a'.repeat(32)}`, source: 'native-executor', observed_at: '2026-10-17T12:00:00Z', recorded_at: '2026-10-20T03:00:00Z', configured: true, items: [{ ...pull, ci_state }], has_more: false };
    const fetch = vi.fn().mockResolvedValueOnce(response(page)); vi.stubGlobal('fetch', fetch);
    if (ci_state === 'not_required' || ci_state === 'unknown') expect((await api.reviewQueue()).items[0].ci_state).toBe(ci_state);
    else await expect(api.reviewQueue()).rejects.toMatchObject({ code: 'invalid_response' });
    expect(fetch).toHaveBeenCalledTimes(1);
  }
  api.close();
});
