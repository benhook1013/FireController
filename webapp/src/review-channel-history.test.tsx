import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import type { ReactNode } from 'react';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Api } from './api';
import { ReviewHistoryGroups, ReviewRoundHistory, ReviewRunsView } from './review-workspace';
import { ReviewArchive } from './review-archive';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewChannel, ReviewRunOptions, ReviewRunSummary, ReviewRuns } from './types';

const retained = vi.hoisted(() => ({ archives: [] as unknown[] }));
vi.mock('./review-archive', async importOriginal => {
  const actual = await importOriginal<typeof import('./review-archive')>();
  return { ...actual, ReviewArchive: class extends actual.ReviewArchive<unknown> {
    constructor(read: ConstructorParameters<typeof actual.ReviewArchive>[0], identity: ConstructorParameters<typeof actual.ReviewArchive>[1]) {
      super(read, identity); retained.archives.push(this);
    }
  } };
});
const key = ['fixture', 'connection', 'instance_one', 'project_one'];
const snapshot = 'review_snapshot_' + 'a'.repeat(32);
const stamp = '2026-10-23T01:00:00Z';
const metadata = { project_id: 'project_one', revision: 1, snapshot_id: snapshot, source: 'sealed-source', observed_at: stamp, recorded_at: stamp };
const totals = { hosted: 2, cli: 40, subagent: 5, manual: 0 };
const clients: Api[] = [];
const caches: QueryClient[] = [];
const run = (channel: ReviewChannel, index: number): ReviewRunSummary => ({ id: 'review_run_' + (index + ({hosted:1000,cli:2000,subagent:3000,manual:4000}[channel])).toString(16).padStart(32,'0'), source: 'sealed-source', source_run_id: channel + '-' + index, pull_number: 2967, channel, head_sha: 'b'.repeat(40), reviewer: 'Reviewer', scope: 'Full comparison', outcome: 'completed', complete: true, attributable: true, finalized: true, found: 1, accepted: 1, routed: 0, fixed: 0, started_at: stamp, finished_at: stamp, coverage_limits: [] });
const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } });
async function fixture() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = new Api('private-fixture-token', 'project_one'); clients.push(api);
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : json({ id: 'project_one', name: 'Project', github_repository_id: 17, repository: 'owner/project' })));
  await api.connect();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); caches.push(client);
  const session = { api, controller: new AbortController() } as Session;
  const render = (content: ReactNode) => renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project_one', selectWorker: () => {}, fail: () => {}, ready: true }}>{content}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
  return { api, client, render };
}
function channelResponse(url: URL): ReviewRuns {
  const channel = url.searchParams.get('channel') as ReviewChannel;
  if (!channel || !Object.hasOwn(totals, channel)) throw new Error('A channel view attempted an unfiltered global history read.');
  const limit = Number(url.searchParams.get('limit'));
  const offset = url.searchParams.has('cursor') ? limit : 0;
  const amount = Math.min(limit, totals[channel] - offset);
  return { ...metadata, items: Array.from({ length: amount }, (_, index) => run(channel, totals[channel] - offset - index)), has_more: offset + amount < totals[channel], ...(offset + amount < totals[channel] ? { cursor: 'only_' + channel + '_continuation' } : {}) };
}
function fetchChannels() {
  const fetch = vi.fn(async (input: RequestInfo | URL) => json(channelResponse(new URL(String(input)))));
  vi.stubGlobal('fetch', fetch); return fetch;
}
afterEach(() => { clients.splice(0).forEach(client => client.close()); caches.splice(0).forEach(cache => cache.clear()); retained.archives = []; vi.unstubAllGlobals(); });

describe('bounded channel review history', () => {
  it('validates channel selectors before network and rejects another channel in a selected response', async () => {
    const { api } = await fixture();
    const fetch = vi.fn(async () => json({ ...metadata, items: [run('cli', 1)], has_more: false })); vi.stubGlobal('fetch', fetch);
    for (const channel of ['', 'all', 'HOSTED', null, 1, ['hosted']]) await expect(api.reviewRuns(2967, { channel } as ReviewRunOptions)).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetch).not.toHaveBeenCalled();
    await expect(api.reviewRuns(2967, { channel: 'hosted', snapshot, limit: 6 })).rejects.toMatchObject({ code: 'invalid_response' });
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it('shares bounded hero/queue channel reads and retrieves old Hosted despite forty newer CLI rounds', async () => {
    const { client, render } = await fixture(); const fetch = fetchChannels();
    const props = { number: 2967, snapshot, head: 'b'.repeat(40), attempts: [], expected: 47, channelTotals: totals };
    render(<><ReviewRunsView {...props} compact /><ReviewRunsView {...props} /></>);
    const queries = client.getQueryCache().getAll();
    expect(queries).toHaveLength(3);
    expect(queries.map(query => query.queryKey.at(-3)).sort()).toEqual(['cli', 'hosted', 'subagent']);
    expect(queries.every(query => query.queryKey.at(-1) === 6)).toBe(true);
    const pages = await Promise.all(queries.map(query => client.fetchQuery({ ...query.options, queryKey: query.queryKey }) as Promise<ReviewRuns>));
    expect(fetch).toHaveBeenCalledTimes(3);
    for (const call of fetch.mock.calls) {
      const url = new URL(String(call[0]));
      expect(url.pathname).toBe('/api/v1/projects/project_one/reviews/pulls/2967/runs');
      expect(url.searchParams.get('snapshot')).toBe(snapshot);
      expect(url.searchParams.get('limit')).toBe('6');
      expect(url.searchParams.has('cursor')).toBe(false);
    }
    const hosted = pages.find(page => page.items[0].channel === 'hosted')!;
    expect(hosted.items.map(item => item.source_run_id)).toEqual(['hosted-2', 'hosted-1']);
    const html = render(<ReviewHistoryGroups number={2967} snapshot={snapshot} head={'b'.repeat(40)} runs={pages.flatMap(page => page.items)} partial loading={false} failed={false} channelTotals={totals} />);
    expect(html).toContain('data-run-id="' + run('hosted', 1).id + '"');
    expect(html).toContain('CLI #40');
    expect(html).not.toContain('PRIVATE_FINDING_BODY');
  });

  it('loads one mixed chronological page and prepends only one earlier page on demand', async () => {
    const { client, render } = await fixture();
    const newest = [run('cli',40), run('hosted',2), ...Array.from({length:23},(_,i)=>run('cli',39-i))];
    const earlier = [run('hosted',1), ...Array.from({length:16},(_,i)=>run('cli',16-i)), ...Array.from({length:5},(_,i)=>run('subagent',5-i))];
    const fetch = vi.fn(async(input: RequestInfo | URL) => {
      const url = new URL(String(input));
      return json({...metadata,items:url.searchParams.has('cursor')?earlier:newest,has_more:!url.searchParams.has('cursor'),...(!url.searchParams.has('cursor')?{cursor:'mixed_continuation'}:{})});
    });
    vi.stubGlobal('fetch',fetch);
    render(<ReviewRoundHistory number={2967} snapshot={snapshot} head={'b'.repeat(40)} expected={47} channelTotals={totals} />);
    const queries = client.getQueryCache().getAll();
    expect(queries).toHaveLength(1);
    expect(queries[0].queryKey).toContain('timeline');
    const page = await client.fetchQuery({...queries[0].options,queryKey:queries[0].queryKey}) as ReviewRuns;
    expect(fetch).toHaveBeenCalledTimes(1);
    const archive = retained.archives[0] as ReviewArchive<ReviewRunSummary>;
    archive.reset(page);
    await archive.more();
    expect(archive.snapshot().items).toHaveLength(47);
    expect(archive.snapshot().items.slice(0,25)).toEqual(newest);
    expect(archive.snapshot().hasMore).toBe(false);
    expect(fetch).toHaveBeenCalledTimes(2);
    for (const call of fetch.mock.calls) {
      const url = new URL(String(call[0]));
      expect(url.searchParams.get('view')).toBe('timeline');
      expect(url.searchParams.get('limit')).toBe('25');
      expect(url.searchParams.has('channel')).toBe(false);
      expect(url.searchParams.get('snapshot')).toBe(snapshot);
    }
    expect(new URL(String(fetch.mock.calls[1][0])).searchParams.get('cursor')).toBe('mixed_continuation');
  });

  it('marks a missing expected channel page unavailable instead of treating it as empty or outside a global preview', async () => {
    const { client, render } = await fixture();
    client.setQueryData([...key, 'reviews', snapshot, 'pull', 2967, 'runs', 'hosted', 'archive-first', 6], { ...metadata, items: [], has_more: false });
    const html = render(<ReviewRunsView number={2967} snapshot={snapshot} head={'b'.repeat(40)} attempts={[]} expected={2} channelTotals={{ hosted: 2, cli: 0, manual: 0, subagent: 0 }} />);
    expect(html).toContain('2 runs');
    expect(html).toContain('Round history could not be loaded.');
    expect(html).not.toContain('No rounds in this preview.');
  });
});
