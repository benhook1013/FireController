import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ApiError } from './api';
import { InlineReviewFindings, ReviewHistoryGroups, ReviewHistoryRow, ReviewRunsView, ReviewAcceptedCounts, reviewDuration } from './review-workspace';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewRunDetail, ReviewRunSummary } from './types';

const key = ['fc', 'connection', 'instance', 'project', 'actor'];
const snapshot = 'snapshot-one';
const run = (id: string, channel: ReviewRunSummary['channel'] = 'cli'): ReviewRunSummary => ({ id, channel, source: 'producer-a', pull_number: 27, source_run_id: `original-${id}`, head_sha: 'head', reviewer: 'Reviewer', scope: 'Selected comparison', outcome: 'completed', complete: true, attributable: true, finalized: true, found: 2, accepted: 1, routed: 1, fixed: 0, started_at: '2026-10-18T00:00:00Z', finished_at: '2026-10-18T00:01:00Z', coverage_limits: [] });
const detail = (value: ReviewRunSummary): ReviewRunDetail => ({ project_id: 'project', revision: 1, snapshot_id: snapshot, source: 'publisher', observed_at: '2026-10-18T00:02:00Z', recorded_at: '2026-10-18T00:02:01Z', pull_number: 27, run: value, findings: [{ source_finding_id: 'finding', title: '**Literal title**', detail: 'Private **finding**\n\n- Useful step\n\n[unsafe](javascript:alert(1)) ![remote](https://outside.invalid/pixel)', severity: 'low', disposition: 'accepted', decision_reason: 'Verified *reason*.', actor: 'Builder', decided_at: '2026-10-18T00:03:00Z', fix_sha: '', proof_note: '`proof`', resolved_at: '2026-10-18T00:04:00Z' }] });
function markup(node: React.ReactNode, seed?: { value: ReviewRunDetail; error?: ApiError; selectedKey?: readonly string[] }) {
  const read = vi.fn();
  const session = { project: { id: 'project', repository: 'example/project' }, controller: new AbortController(), api: { reviewRun: read } } as unknown as Session;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (seed) {
    const queryKey = [...(seed.selectedKey ?? key), 'reviews', seed.value.snapshot_id, 'inline-run', seed.value.run.id];
    client.setQueryData(queryKey, seed.value);
    if (seed.error) client.getQueryCache().find({ queryKey })!.setState({ status: 'error', error: seed.error });
  }
  try {
    const html = renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project', ready: true, fail: () => {}, selectWorker: () => {} }}>{node}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
    expect(read).not.toHaveBeenCalled();
    return html;
  } finally { client.clear(); }
}

describe('per-PR inline retained review history', () => {
  it('renders decision and fix times as relative time elements with their exact recorded instants', () => {
    const value = run('dated');
    const html = markup(<InlineReviewFindings number={27} run={value} snapshot={snapshot} />, { value: detail(value) });
    expect(html).toContain('dateTime="2026-10-18T00:03:00Z"');
    expect(html).toContain('dateTime="2026-10-18T00:04:00Z"');
    expect(html).toContain('Resolved: <time');
  });

  it('shows typed compact summaries, finish age and pinned secondary links without exposing cached findings before expansion', () => {
    const value = run('second');
    const html = markup(<ReviewHistoryRow number={27} run={value} snapshot={snapshot} head="head" />, { value: detail(value) });
    expect(html).toContain('>CodeRabbit CLI</span>'); expect(html).toContain('Found: <strong>2</strong>');
    expect(html).toContain('Accepted: <strong>1</strong>'); expect(html).toContain('Routed: <strong>1</strong>');
    expect(html).toContain('dateTime="2026-10-18T00:01:00Z"'); expect(html).toContain('aria-expanded="false"');
    expect(html).toContain('snapshot=snapshot-one'); expect(html).toContain('Run details ↗');
    expect(html).not.toContain('Private'); expect(html).not.toContain('review-history-findings"><');
    expect(html).not.toMatch(/Ready to merge|Round 1/u);
  });
  it('interleaves channels in supplied chronology and numbers each channel across a partial newest prefix', () => {
    const items = [run('last'), run('hosted', 'hosted'), { ...run('previous'), source: 'producer-b', source_run_id: 'original-first' }, run('manual', 'manual')];
    const html = markup(<ReviewHistoryGroups number={27} runs={items} snapshot={snapshot} head="head" partial loading={false} failed={false} channelTotals={{hosted: 5, cli: 8, manual: 1, subagent: 0}} />);
    const positions = ['manual', 'previous', 'hosted', 'last'].map(id => html.indexOf(`data-run-id="${id}"`));
    expect(positions.every((value, index) => index === 0 || value > positions[index - 1])).toBe(true);
    for (const label of ['CodeRabbit CLI #8', 'CodeRabbit CLI #7', 'CodeRabbit Hosted #5', 'Manual #1']) expect(html).toContain(label);
    expect(html).not.toContain('original-first');
    expect(html.match(/<ul class="review-history-run-list">/gu)).toHaveLength(1);
  });
  it('keeps capitalized Subagent Reviews in queue lanes and compact Subagent round labels in the mixed timeline', () => {
    const totals = { hosted: 0, cli: 0, manual: 0, subagent: 1 };
    const history = markup(<ReviewHistoryGroups number={27} runs={[run('subagent', 'subagent')]} snapshot={snapshot} head="head" partial={false} loading={false} failed={false} channelTotals={totals} />);
    const lanes = markup(<ReviewRunsView number={27} snapshot={snapshot} head="head" attempts={[]} expected={1} channelTotals={totals} />);
    expect(history).toContain('Subagent #1');
    expect(lanes).toContain('<h4>Subagent Reviews</h4>');
    expect(lanes).not.toContain('Subagent reviews');
  });
  it('shows captured accepted severities without fabricating missing breakdowns', () => {
    const value = { ...run('counts'), accepted: 4, accepted_severity: {critical: 1, major: 1, minor: 1, trivial: 0, unknown: 1, complete: true} };
    const html = markup(<ReviewAcceptedCounts run={value} />);
    for (const text of ['Critical', 'Major / Minor / Trivial', 'severity unknown']) expect(html).toContain(text);
    expect(html).toContain('4 accepted findings');
    expect(html).toContain('Accepted: <strong>1/1/0</strong>');
    expect(html).not.toContain('review-severity-high');
    const incomplete = markup(<ReviewAcceptedCounts run={{...value, accepted_severity:{...value.accepted_severity,complete:false}}} />);
    expect(incomplete).toContain('Accepted: <strong>4</strong>');
    expect(incomplete).not.toContain('Major');
  });
  it('shows review duration only when original timing is known and ordered', () => {
    expect(reviewDuration(run('timed'))).toBe('1m 0s');
    expect(reviewDuration({...run('unknown'), started_at:''})).toBeUndefined();
    expect(reviewDuration({...run('reversed'), started_at:'2026-10-19T00:00:00Z'})).toBeUndefined();
  });
  it('keeps evidence qualification without treating later Git changes as lost progress', () => {
    const value = { ...run('pending'), outcome: 'failed' as const, complete: false, attributable: false, finalized: false, finished_at: '' };
    const html = markup(<ReviewHistoryRow number={27} run={value} snapshot={snapshot} head="other-head" />);
    for (const text of ['Incomplete record', 'Counts not finalized', 'Attribution unconfirmed', 'Started']) expect(html).toContain(text);
    expect(html).not.toContain('Different observed head');
    const missing = markup(<ReviewHistoryRow number={27} run={{ ...value, started_at: '', head_sha: '' }} snapshot={snapshot} head="head" />);
    expect(missing).toContain('Date unknown'); expect(missing).toContain('Head association unknown');
  });
  it('keeps partial and failed histories distinct from confirmed empty history', () => {
    const props = { number: 27, runs: [], snapshot, head: 'head', loading: false, failed: false, channelTotals: {hosted: 0, cli: 0, manual: 0, subagent: 0} };
    expect(markup(<ReviewHistoryGroups {...props} partial channelTotals={{hosted: 27, cli: 1, manual: 8, subagent: 2}} />)).toContain('Round history could not be loaded.');
    expect(markup(<ReviewHistoryGroups {...props} partial={false} />)).toContain('No rounds recorded. Coverage is unknown.');
    expect(markup(<ReviewHistoryGroups {...props} partial failed />)).toContain('Round history could not be loaded.');
    expect(markup(<ReviewHistoryGroups {...props} partial loading />)).toContain('Loading retained rounds');
  });
  it('renders actual inline finding, decision and proof Markdown through the safe renderer', () => {
    const value = { ...run('expanded'), found: 1, accepted: 1, routed: 0 };
    const html = markup(<InlineReviewFindings number={27} run={value} snapshot={snapshot} />, { value: detail(value) });
    expect(html).toContain('1 recorded / 1 reported findings'); expect(html).toContain('**Literal title**');
    expect(html).toContain('<strong>finding</strong>'); expect(html).toContain('<li>Useful step</li>');
    expect(html).toContain('<em>reason</em>'); expect(html).toContain('<code>proof</code>');
    expect(html).not.toContain('<img'); expect(html).not.toContain('href="javascript:');
  });
  it('distinguishes zero findings from an incomplete snapshot with missing finding text', () => {
    const zero = { ...run('zero'), found: 0, accepted: 0, routed: 0 };
    expect(markup(<InlineReviewFindings number={27} run={zero} snapshot={snapshot} />, { value: { ...detail(zero), findings: [] } })).toContain('No findings reported for this run.');
    const incomplete = { ...run('incomplete'), complete: false, finalized: false };
    const html = markup(<InlineReviewFindings number={27} run={incomplete} snapshot={snapshot} />, { value: { ...detail(incomplete), findings: [] } });
    expect(html).toContain('No findings recorded in this source snapshot. The source record is incomplete.');
  });
  it('suppresses retained private details on terminal errors and separates project and snapshot query scope', () => {
    const value = run('isolated');
    const node = <InlineReviewFindings number={27} run={value} snapshot={snapshot} />;
    const denied = markup(node, { value: detail(value), error: new ApiError('forbidden', 'Denied', 403) });
    expect(denied).toContain('Access ended.'); expect(denied).not.toContain('Private'); expect(denied).not.toContain('Retry findings');
    expect(markup(node, { value: detail(value), selectedKey: ['fc', 'other-connection', 'instance', 'other-project', 'actor'] })).not.toContain('Private');
    expect(markup(<InlineReviewFindings number={27} run={value} snapshot="another-snapshot" />, { value: detail(value) })).not.toContain('Private');
    const failed = markup(node, { value: detail(value), error: new ApiError('connection_failed', 'Offline') });
    expect(failed).toContain('Retry findings'); expect(failed).not.toContain('Private');
  });
});
