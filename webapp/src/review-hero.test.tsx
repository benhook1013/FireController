import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ReviewAttempts, ReviewQueueRow, ReviewRunsView } from './review-workspace';
import { MergeHeadContent } from './delivery';
import type { ReactNode } from 'react';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewAttemptItem, ReviewAttemptSummary, ReviewRunCounts, ReviewRunSummary, ReviewPullSummary, ReviewQueue } from './types';
import type { ReviewRoundProgress } from './review-progress';

// Start at the loaded view boundary, in native newest-first producer order.
// The mounted proof separately exercises real reads and shared query caching.
const archive = vi.hoisted(() => ({ items: [] as (ReviewRunSummary | ReviewAttemptItem)[], hasMore: false, busy: false, error: undefined }));
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
function markup(compact = true, attempts: ReviewAttemptSummary[] = [], expanded = false, channelTotals: ReviewRunCounts = { hosted: archive.items.filter(item => item.channel === 'hosted').length, cli: archive.items.filter(item => item.channel === 'cli').length, manual: 0, subagent: 0 }, content?: ReactNode) {
  const reviewRuns = vi.fn();
  const reviewAttempts = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const session = { project: {id: 'project', repository: 'example/project'}, api: { reviewRuns, reviewAttempts }, controller: new AbortController() } as unknown as Session;
  try {
    return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key: ['fixture'], home: '/projects/project', ready: true, fail: () => {}, selectWorker: () => {} }}>{content ?? (expanded ? <ReviewAttempts number={7} snapshot="snapshot" expected={archive.items.length} /> : <ReviewRunsView number={7} snapshot="snapshot" head="head" attempts={attempts} expected={archive.items.length} compact={compact} channelTotals={channelTotals} progress={{ pull_number: 7, channels: [progress] }} />)}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
  } finally { expect(reviewRuns).not.toHaveBeenCalled(); expect(reviewAttempts).not.toHaveBeenCalled(); client.clear(); }
}
function links(html: string) { return [...html.matchAll(/aria-label="Open (?:hosted|cli) run ([^:]+):/gu)].map(match => match[1]); }

describe('merge-head recent review rounds', () => {
  it('shows the latest three independently per channel in producer order, retaining totals and limits', () => {
    archive.items = [run('h5', 'hosted'), run('h4', 'hosted'), run('c4', 'cli'), run('h3', 'hosted'), run('c3', 'cli'), run('c2', 'cli'), run('h2', 'hosted'), run('c1', 'cli'), run('h1', 'hosted')];
    archive.hasMore = false;
    const html = markup();
    expect(links(html)).toEqual(['h3', 'h4', 'h5', 'c2', 'c3', 'c4']);
    expect(html).toContain('5 runs · latest 3'); expect(html).toContain('4 runs · latest 3');
    expect(html).not.toContain('Up to 3 more reviews'); expect(html).not.toContain('5 of 8 used'); expect(html).toContain('Taper met');
    expect(html).toContain('<strong>5/2/1</strong>'); expect(html).not.toContain('Load more');
    expect(links(markup(false))).toEqual(['h1', 'h2', 'h3', 'h4', 'h5', 'c1', 'c2', 'c3', 'c4']);
  });
  it('renders the actual normal PR item in the hero with the same six-round preview and contextual footer', () => {
    archive.items = Array.from({length: 8}, (_, index) => run(`h${8-index}`, 'hosted'));
    archive.hasMore = true;
    const counts = {hosted: 27, cli: 1, manual: 8, subagent: 2};
    const pull: ReviewPullSummary = {number: 7, title: '<Literal PR title>', state: 'open', head_sha: 'head', base_ref: 'main', base_sha: 'base', ci_state: 'success', mergeability: 'mergeable', complete: true, run_count: 38, run_counts: counts, attempt_count: 0, latest_attempts: [], recent_runs: [], diff: {files: 3, additions: 20, deletions: 4}};
    const queue: ReviewQueue = {project_id: 'project', revision: 1, snapshot_id: 'snapshot', source: 'fixture', observed_at: '', recorded_at: '', configured: true, items: [pull], has_more: false};
    const normal = markup(false, [], false, counts, <ul className="review-queue"><ReviewQueueRow pull={pull} snapshot="snapshot" order={{index: 0, distance: 1}} /></ul>);
    const hero = markup(false, [], false, counts, <MergeHeadContent queue={queue} home="/projects/project" />);
    const body = (value: string) => value.slice(value.indexOf('<div class="review-queue-content">'), value.lastIndexOf('<a class="review-details-link"')).replace(/ data-reading-anchor="[^"]*"/gu, '');
    expect(body(hero)).toBe(body(normal));
    expect(links(hero)).toEqual(['h3','h4','h5','h6','h7','h8']);
    expect(hero).toContain('27 runs</span>'); expect(hero).toContain('Manual reviews'); expect(hero).toContain('8 runs</span>');
    expect(hero).not.toContain(' · latest 3'); expect(hero).not.toContain('delivery-head-history');
    expect(hero).not.toContain('review-queue-position'); expect(hero).not.toContain('id="review-pr-7"');
    expect(normal).toContain('review-queue-position'); expect(normal).toContain('id="review-pr-7"');
    expect(hero).toContain('&lt;Literal PR title&gt;');
    for (const value of [normal, hero]) {
      expect(value).toContain('class="badge review-fact-pill review-diff"');
      expect(value).toContain('class="badge review-fact-pill review-ci review-fact-success"');
      expect(value).toContain('class="badge review-fact-pill review-mergeability review-fact-mergeable"');
      expect(value).toContain('review-additions">+20'); expect(value).toContain('review-deletions">−4');
    }
    expect(hero).toContain('href="/projects/project#review-pr-7"'); expect(hero).toContain('>Review queue ↗</a>');
    expect(hero).not.toContain('Review details ↗'); expect(normal).toContain('Review details ↗');
  });
  it('caps queue previews independently at the newest six without losing producer order or totals', () => {
    archive.items = Array.from({length:8}, (_, index) => 8-index).flatMap(index => [run(`h${index}`, 'hosted'), run(`c${index}`, 'cli')]);
    archive.hasMore = false;
    const html = markup(false);
    expect(links(html)).toEqual(['h3','h4','h5','h6','h7','h8','c3','c4','c5','c6','c7','c8']);
    expect(html).toContain('8 runs</span>'); expect(html).not.toContain(' · recent'); expect(html).not.toContain('Load more');
  });
  it('uses exact supplied channel totals independently of the fetched preview', () => {
    archive.items = [run('h4', 'hosted'), run('h3', 'hosted'), run('h2', 'hosted'), run('h1', 'hosted')];
    archive.hasMore = true;
    const totals = { hosted: 27, cli: 1, manual: 8, subagent: 2 };
    const hero = markup(true, [], false, totals);
    expect(hero).toContain('27 runs · latest 3');
    expect(hero).toContain('1 run</span>');
    expect(hero).not.toContain('4 runs loaded');
    expect(links(hero)).toEqual(['h2', 'h3', 'h4']);
    const queue = markup(false, [], false, totals);
    for (const label of ['27 runs', '1 run', '8 runs', '2 runs']) expect(queue).toContain(`${label}</span>`);
    expect(queue).not.toContain(' · latest 3');
    expect(queue).not.toContain(' loaded</span>');
    expect(queue).toContain('Round history could not be loaded.');
    expect(links(queue)).toEqual(['h1', 'h2', 'h3', 'h4']);
  });
  it('keeps partial coverage, absent channels and unknown attribution honest without fetching more', () => {
    archive.items = [run('unknown', 'hosted', { head_sha: '', finished_at: '', complete: false, attributable: false, finalized: false }), run('h3', 'hosted'), run('h2', 'hosted'), run('h1', 'hosted')];
    archive.hasMore = true;
    const html = markup();
    expect(links(html)).toEqual(['h2', 'h3', 'unknown']);
    expect(html).toContain('4 runs · latest 3'); expect(html).not.toContain(' loaded</span>'); expect(html).toContain('No rounds recorded. Coverage is unknown.');
    for (const text of ['Date unknown', 'Incomplete record', 'Counts not finalized', 'Attribution unconfirmed', 'Head association unknown']) expect(html).toContain(text);
    expect(html).not.toContain('Current cli round limits'); expect(html).not.toContain('Load more');
  });
});

describe('retained attempt diagnostics', () => {
  const attempt: ReviewAttemptItem = { source: 'native', source_attempt_id: 'attempt_one', channel: 'hosted', head_sha: 'head', state: 'uncertain', started_at: '', finished_at: '', duration_seconds: null, source_run_id: '', trigger_id: '', provider_review_id: '' };
  it.each([
    ['uncertain', 'hosted_finished_inventory_unproven', 'Uncertain', 'Finished response could not be verified'],
    ['held', 'hosted_allowance_cooldown', 'Held', 'Waiting for hosted allowance'],
    ['rate_limited', 'allowance_exhausted', 'Cooldown', 'CLI allowance exhausted'],
    ['failed', 'another_safe_code', 'Failed', 'Another safe code'],
    ['failed', 'constructor', 'Failed', 'Constructor'],
  ] as const)('shows %s reason in the same compact pill and expanded attempt without fetching', (state, reason, label, text) => {
    const value = { ...attempt, state, reason };
    archive.items = []; archive.hasMore = false;
    const compact = markup(true, [value]);
    expect(compact).toContain(`aria-label="Latest retained hosted attempt: ${label} · ${text}"`);
    expect(compact).toContain(`title="Latest retained attempt: ${label} · ${text}"`);
    expect(compact).toContain(`>${label}</span>`);
    expect(compact.match(/class="review-attempt-state /gu)).toHaveLength(1);
    archive.items = [value];
    const expanded = markup(false, [], true);
    expect(expanded).toContain(`<small>Reason</small><span title="${reason}">${text}</span>`);
    expect(expanded).toContain('Attempts (1)');
  });
  it('keeps old absent reasons and non-diagnostic states free of added prose', () => {
    for (const state of ['held', 'uncertain', 'active', 'completed'] as const) {
      const value = { ...attempt, state };
      archive.items = [];
      const compact = markup(true, [value]);
      expect(compact).not.toContain(' · Finished response');
      archive.items = [value];
      expect(markup(false, [], true)).not.toContain('<small>Reason</small>');
    }
    for (const state of ['active', 'completed'] as const) {
      const value = { ...attempt, state, reason: 'hosted_finished_inventory_unproven' };
      archive.items = [];
      expect(markup(true, [value])).not.toContain('Finished response could not be verified');
      archive.items = [value];
      expect(markup(false, [], true)).not.toContain('<small>Reason</small>');
    }
  });
});
