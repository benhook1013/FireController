import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Api } from './api';
import { ReviewHistoryGroups, ReviewRunsView, readCompletedRunPreview } from './review-workspace';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewRunSummary } from './types';

const archive = vi.hoisted(() => ({ items: [] as ReviewRunSummary[], hasMore: false, busy: false, error: undefined }));
vi.mock('./review-archive', async importOriginal => {
  const actual = await importOriginal<typeof import('./review-archive')>();
  return { ...actual, ReviewArchive: class { snapshot = () => archive; subscribe = () => () => {}; } };
});
const head = 'a'.repeat(40), snapshot = 'review_snapshot_' + 'b'.repeat(32);
const project = { id: 'project_one', name: 'Project', github_repository_id: 1, repository: 'owner/repo' };
const stamp = '2026-11-01T00:00:00Z';
const metadata = { project_id: project.id, snapshot_id: snapshot, revision: 1, source: 'fixture', observed_at: stamp, recorded_at: stamp };
const totals = { cli: 6, hosted: 0, subagent: 0, manual: 0 };
function run(index: number, changes: Partial<ReviewRunSummary> = {}): ReviewRunSummary { return { id: 'review_run_' + index.toString(16).padStart(32, '0'), source: 'fixture', source_run_id: `completed_${index}`, pull_number: 3038, channel: 'cli', head_sha: head, reviewer: 'Reviewer', scope: 'Comparison', coverage_limits: [], outcome: 'completed', complete: true, attributable: true, finalized: true, found: 0, accepted: 0, routed: 0, fixed: 0, started_at: stamp, finished_at: stamp, ...changes }; }
const failed = (index: number) => run(index, { source_run_id: `failed_${index}`, outcome: 'failed', complete: false, finalized: false });
const mixed = [run(6), failed(8), run(5, { finalized: false }), failed(7), run(4), run(3), run(2), run(1)];
const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } });
const clients: Api[] = [];
afterEach(() => { clients.splice(0).forEach(api => api.close()); vi.unstubAllGlobals(); archive.items = []; });
function render(compact: boolean, history = false, limited = false) {
  archive.items = limited ? [] : mixed;
  const client = new QueryClient();
  if (limited) client.setQueryData(['fixture', 'reviews', snapshot, 'pull', 3038, 'runs', 'cli', 'archive-first', 6], { ...metadata, items: [], has_more: true, cursor: 'next', previewLimited: true });
  const session = { api: { reviewRuns: vi.fn() }, controller: new AbortController() } as unknown as Session;
  try { return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key: ['fixture'], home: '/projects/project_one', ready: true, fail: () => {}, selectWorker: () => {} }}>{history ? <ReviewHistoryGroups number={3038} runs={mixed} snapshot={snapshot} head={head} partial={false} loading={false} failed={false} channelTotals={totals} /> : <ReviewRunsView number={3038} snapshot={snapshot} head={head} attempts={[]} expected={8} channelTotals={totals} compact={compact} />}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>); }
  finally { client.clear(); }
}
function pills(html: string) { return [...html.matchAll(/aria-label="Open cli run ([^:]+):/gu)].map(match => match[1]); }

describe('completed round previews retain failed-start history separately', () => {
  it('shows six actual runs from eight raw records and caps after filtering failures', () => {
    const compact = render(true), queue = render(false);
    expect(compact).toContain('6 runs'); expect(compact).not.toContain('8 runs');
    expect(pills(compact)).toEqual(['completed_1', 'completed_2', 'completed_3', 'completed_4', 'completed_5', 'completed_6']);
    expect(pills(queue)).toEqual(['completed_1', 'completed_2', 'completed_3', 'completed_4', 'completed_5', 'completed_6']);
    expect(queue).toContain('review-pill-zero'); expect(queue).not.toContain('Counts not finalized'); expect(queue).toContain('<strong>0/?/?</strong>');
    for (const html of [compact, queue]) { expect(html).not.toMatch(/failed_7|failed_8|review-pill-failed/u); }
  });
  it('retains failed capture diagnostics in full history without consuming completed-round ordinals', () => {
    const html = render(false, true);
    const failedRow = html.split(`data-run-id="${failed(8).id}"`)[1].split('</li>')[0];
    const completedRow = html.split(`data-run-id="${run(5).id}"`)[1].split('</li>')[0];
    expect(failedRow).toContain('Failed'); expect(failedRow).not.toContain('CLI #');
    expect(completedRow).toContain('CLI #5');
    expect(html.match(/class="review-history-row /gu)).toHaveLength(8);
  });
  it('refills from raw channel pages until six completed rounds, preserving pinned inputs', async () => {
    const before = JSON.stringify(mixed), calls: (string | undefined)[] = [];
    const page = await readCompletedRunPreview(async cursor => {
      calls.push(cursor);
      return { ...metadata, items: cursor ? mixed.slice(6) : mixed.slice(0, 6), has_more: !cursor, ...(!cursor ? { cursor: 'next_six' } : {}) };
    }, new AbortController().signal);
    expect(calls).toEqual([undefined, 'next_six']);
    expect(page.items.map(item => item.source_run_id)).toEqual(['completed_6', 'completed_5', 'completed_4', 'completed_3', 'completed_2', 'completed_1']);
    expect(page.has_more).toBe(false); expect(page.previewLimited).toBeUndefined(); expect(JSON.stringify(mixed)).toBe(before);
  });
  it('fills a width-sized preview beyond six and retains bounded failed-start refills', async () => {
    let reads = 0;
    const page = await readCompletedRunPreview(async () => {
      const start = reads++ * 8;
      return { ...metadata, items: Array.from({ length: 8 }, (_, i) => i === 0 ? failed(100 + start) : run(100 - start - i)), has_more: true, cursor: `page_${reads}` };
    }, new AbortController().signal, 8);
    expect(reads).toBe(2); expect(page.items).toHaveLength(8);
    expect(page.items.every(item => item.outcome === 'completed')).toBe(true);
  });
  it('stops after eighteen raw records and marks the preview limited instead of walking growing history', async () => {
    let reads = 0;
    const page = await readCompletedRunPreview(async () => {
      const start = reads++ * 6;
      return { ...metadata, items: Array.from({ length: 6 }, (_, index) => failed(100 + start + index)), has_more: true, cursor: `page_${reads}` };
    }, new AbortController().signal);
    expect(reads).toBe(3); expect(page.items).toEqual([]); expect(page.previewLimited).toBe(true); expect(page.has_more).toBe(true);
  });
  it('links to retained history when the bounded preview cannot fill its completed slots', () => {
    const html = render(false, false, true);
    expect(html).toContain('Recent-round preview is limited.');
    expect(html).not.toContain('Round history could not be loaded.');
    expect(html).toContain(`/reviews/pulls/3038?snapshot=${snapshot}`);
  });
  it('uses the actual channel API with six-item requests and one snapshot-bound refill', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : json(project)));
    const api = new Api('private-token', project.id); clients.push(api); await api.connect();
    const urls: URL[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input)); urls.push(url);
      return json({ ...metadata, items: url.searchParams.has('cursor') ? mixed.slice(6) : mixed.slice(0, 6), has_more: !url.searchParams.has('cursor'), ...(!url.searchParams.has('cursor') ? { cursor: 'next_six' } : {}) });
    }));
    const client = new QueryClient(), session = { api, controller: new AbortController() } as Session;
    try {
      renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key: ['transport'], home: '/projects/project_one', ready: true, fail: () => {}, selectWorker: () => {} }}><ReviewRunsView number={3038} snapshot={snapshot} head={head} attempts={[]} expected={8} channelTotals={totals} /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
      const query = client.getQueryCache().getAll().find(query => query.queryKey.includes('cli'))!;
      const page = await client.fetchQuery({ ...query.options, queryKey: query.queryKey }) as { items: ReviewRunSummary[] };
      expect(page.items.map(item => item.source_run_id)).toEqual(['completed_6', 'completed_5', 'completed_4', 'completed_3', 'completed_2', 'completed_1']);
      expect(urls).toHaveLength(2);
      for (const url of urls) { expect(url.pathname).toBe(`/api/v1/projects/${project.id}/reviews/pulls/3038/runs`); expect(url.searchParams.get('channel')).toBe('cli'); expect(url.searchParams.get('limit')).toBe('6'); expect(url.searchParams.get('snapshot')).toBe(snapshot); }
      expect(urls.map(url => url.searchParams.get('cursor'))).toEqual([null, 'next_six']);
    } finally { client.clear(); }
  });
  it('does not refill a complete first preview or true history end, and rejects inconsistent continuations', async () => {
    for (const items of [Array.from({ length: 6 }, (_, index) => run(6 - index)), [failed(7), run(1)]]) {
      const read = vi.fn(async () => ({ ...metadata, items, has_more: items.length === 6, ...(items.length === 6 ? { cursor: 'unused' } : {}) }));
      const page = await readCompletedRunPreview(read, new AbortController().signal);
      expect(read).toHaveBeenCalledTimes(1); expect(page.previewLimited).toBeUndefined();
    }
    for (const change of [{ source: 'foreign' }, { snapshot_id: 'different_snapshot' }, { cursor: 'first_cursor' }, { items: [failed(10)] }]) {
      let calls = 0;
      await expect(readCompletedRunPreview(async () => ++calls === 1 ? { ...metadata, items: [failed(10)], has_more: true, cursor: 'first_cursor' } : { ...metadata, items: [run(1)], has_more: true, cursor: 'second_cursor', ...change }, new AbortController().signal)).rejects.toMatchObject({ code: 'invalid_response' });
      expect(calls).toBe(2);
    }
    const controller = new AbortController();
    const read = vi.fn(async () => { controller.abort(); return { ...metadata, items: [failed(10)], has_more: true, cursor: 'unused' }; });
    await expect(readCompletedRunPreview(read, controller.signal)).rejects.toMatchObject({ name: 'AbortError' }); expect(read).toHaveBeenCalledTimes(1);
  });
  it('accepts raw history totals above actual completed counts but rejects invalid channel counts', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : json(project)));
    const api = new Api('private-token', project.id); clients.push(api); await api.connect();
    const pull = { number: 3038, title: 'Retained PR', state: 'open', head_sha: head, base_ref: 'main', base_sha: 'c'.repeat(40), ci_state: 'unknown', mergeability: 'unknown', complete: false, run_count: 8, run_counts: totals, attempt_count: 0, recent_runs: mixed.slice(0, 3), latest_attempts: [] };
    for (const value of [pull, { ...pull, run_count: 2, run_counts: { ...totals, cli: 0 }, recent_runs: [failed(8), failed(7)] }]) {
      vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, pull: value })));
      expect((await api.reviewPull(3038, snapshot)).pull).toEqual(value);
    }
    for (const cli of [9, -1, 1.5, null, '6', Number.MAX_SAFE_INTEGER + 1]) {
      vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, pull: { ...pull, run_counts: { ...totals, cli } } })));
      await expect(api.reviewPull(3038, snapshot)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    vi.stubGlobal('fetch', vi.fn(async () => json({ ...metadata, pull: { ...pull, run_counts: { ...totals, cli: 0 } } })));
    await expect(api.reviewPull(3038, snapshot)).rejects.toMatchObject({ code: 'invalid_response' });
  });
});
