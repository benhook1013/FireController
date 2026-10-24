import { afterEach, expect, it, vi } from 'vitest';
import { Api } from './api';
import { validRoundProgress } from './review-progress';

const channel = { channel: 'hosted', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 5, taper: 1, used: 0, dry: 0, tapered: false, pending: 3 };
const absent = { ...channel, channel: 'cli', configured: false, revision: 0, epoch: 0, maximum: 0, taper: 0, pending: 0 };
const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } });
afterEach(() => vi.unstubAllGlobals());

it('accepts conserved execution and adjudication counts and unchanged legacy fields', () => {
  for (const value of [channel, { ...channel, pending_execution: 2, pending_adjudication: 1 }, { ...channel, pending_execution: 0, pending_adjudication: 3 }, { ...absent, pending_execution: 0, pending_adjudication: 0 }]) expect(validRoundProgress(value, value.channel)).toBe(true);
  for (const fields of [
    { pending_execution: 3 }, { pending_adjudication: 3 },
    { pending_execution: 1, pending_adjudication: 1 }, { pending_execution: 3, pending_adjudication: 1 },
    { pending_execution: -1, pending_adjudication: 4 }, { pending_execution: 1.5, pending_adjudication: 1.5 },
    { pending_execution: null, pending_adjudication: 3 }, { pending_execution: '0', pending_adjudication: 3 },
    { pending_execution: Number.MAX_SAFE_INTEGER + 1, pending_adjudication: 0 },
  ]) expect(validRoundProgress({ ...channel, ...fields }, 'hosted')).toBe(false);
});

it('requests the staged view once and accepts mixed staged and legacy channel fixtures', async () => {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = new Api('private-test-token', 'project_one');
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : json({ id: 'project_one', name: 'Project', github_repository_id: 1, repository: 'owner/repository' })));
  await api.connect();
  const result = { project_id: 'project_one', evaluated_at: '2026-10-25T00:00:00Z', items: [{ pull_number: 31, channels: [{ ...channel, pending_execution: 2, pending_adjudication: 1 }, absent] }] };
  const fetch = vi.fn(async (_input: RequestInfo | URL) => json(result)); vi.stubGlobal('fetch', fetch);
  try {
    expect(await api.reviewProgress([31])).toEqual(result);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(String(fetch.mock.calls[0][0])).toBe('https://service.example/api/v1/projects/project_one/reviews/rounds/progress?pull_numbers=31&view=stages');
  } finally { api.close(); }
});
