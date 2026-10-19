import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ReviewRunsView } from './review-workspace';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewRunSummary } from './types';
import type { ReviewRoundProgress } from './review-progress';

// Start at the loaded view boundary, in native newest-first producer order.
// The mounted proof separately exercises real reads and shared query caching.
const archive = vi.hoisted(() => ({ items: [] as ReviewRunSummary[], hasMore: false, busy: false, error: undefined }));
vi.mock('./review-archive', async importOriginal => {
  const actual = await importOriginal<typeof import('./review-archive')>();
  return { ...actual, ReviewArchive: class {
    snapshot = () => archive;
    subscribe = () => () => {};
  } };
});
const run = (id: string, channel: 'hosted' | 'cli', changes: Partial<ReviewRunSummary> = {}): ReviewRunSummary => ({
  id, source: 'fixture', source_run_id: id, pull_number: 7, channel, head_sha: 'head', reviewer: 'Reviewer', scope: 'full',
  outcome: 'completed', complete: true, attributable: true, finalized: true, found: 5, accepted: 2, routed: 1, fixed: 0,
  started_at: '', finished_at: '2026-10-19T08:00:00Z', coverage_limits: [], ...changes,
});
const progress: ReviewRoundProgress = { channel: 'hosted', configured: true, revision: 1, epoch: 1, closed: false,
  minimum: 0, maximum: 8, taper: 2, used: 5, dry: 2, tapered: true, pending: 0 };
function markup(compact = true) {
  const reviewRuns = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const session = { api: { reviewRuns }, controller: new AbortController() } as unknown as Session;
  try {
    return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key: ['fixture'], home: '/projects/project', ready: true, fail: () => {}, selectWorker: () => {} }}><ReviewRunsView number={7} snapshot="snapshot" head="head" attempts={[]} expected={archive.items.length} compact={compact} progress={{ pull_number: 7, channels: [progress] }} /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
  } finally { expect(reviewRuns).not.toHaveBeenCalled(); client.clear(); }
}
function links(html: string) { return [...html.matchAll(/aria-label="Open (?:hosted|cli) run ([^:]+):/gu)].map(match => match[1]); }

describe('merge-head recent review rounds', () => {
  it('shows the latest three independently per channel in producer order, retaining totals and limits', () => {
    archive.items = [run('h5', 'hosted'), run('h4', 'hosted'), run('c4', 'cli'), run('h3', 'hosted'), run('c3', 'cli'), run('c2', 'cli'), run('h2', 'hosted'), run('c1', 'cli'), run('h1', 'hosted')];
    archive.hasMore = false;
    const html = markup();
    expect(links(html)).toEqual(['h3', 'h4', 'h5', 'c2', 'c3', 'c4']);
    expect(html).toContain('5 runs · latest 3'); expect(html).toContain('4 runs · latest 3');
    expect(html).toContain('5 of 8 used · Max 3 remaining'); expect(html).toContain('Taper 2/2'); expect(html).toContain('Taper met');
    expect(html).toContain('<strong>5/2/1</strong>'); expect(html).not.toContain('Load more');
    expect(links(markup(false))).toEqual(['h1', 'h2', 'h3', 'h4', 'h5', 'c1', 'c2', 'c3', 'c4']);
  });
  it('keeps partial coverage, absent channels and unknown attribution honest without fetching more', () => {
    archive.items = [run('unknown', 'hosted', { head_sha: '', finished_at: '', complete: false, attributable: false, finalized: false }), run('h3', 'hosted'), run('h2', 'hosted'), run('h1', 'hosted')];
    archive.hasMore = true;
    const html = markup();
    expect(links(html)).toEqual(['h2', 'h3', 'unknown']);
    expect(html).toContain('4 runs loaded · latest 3'); expect(html).toContain('No rounds in loaded pages. Coverage is unknown.');
    for (const text of ['Date unknown', 'Incomplete record', 'Counts not finalized', 'Attribution unconfirmed', 'Head association unknown']) expect(html).toContain(text);
    expect(html).not.toContain('Current cli round limits'); expect(html).not.toContain('Load more');
  });
});
