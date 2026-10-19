import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { WorkspaceContext, type Session } from './workspace';
import { PresentationContext } from './presentation-context';
import { ReviewFindingView, ReviewPullFacts, ReviewQueueView, ReviewRunsView, formatQueueDistance, groupReviewQueue, missingPullFacts, reviewQueueDistances } from './review-workspace';
import type { Presentation, ReviewAttemptSummary, ReviewFinding, ReviewPullSummary, ReviewQueue } from './types';

const key = ['fc', 'connection', 'instance', 'project', 'actor'];
const pull = (number: number, state: ReviewPullSummary['state'] = 'open'): ReviewPullSummary => ({ number, title: `Change ${number}`, state, head_sha: 'a'.repeat(40), base_ref: 'main', base_sha: 'b'.repeat(40), ci_state: 'success', mergeability: 'mergeable', complete: true, run_count: 0, attempt_count: 0, recent_runs: [], latest_attempts: [] });
function markup(node: React.ReactNode, queue?: ReviewQueue, presentation?: Presentation) {
  const api = { reviewQueue: vi.fn(), reviewRuns: vi.fn() };
  const session = { api, controller: new AbortController(), project: { repository: 'example/project' } } as unknown as Session;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (queue) client.setQueryData([...key, 'reviews', 'current', 'queue', ''], queue);
  try {
    const html = renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><PresentationContext.Provider value={presentation}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project', selectWorker: () => {}, fail: () => {}, ready: true }}>{node}</WorkspaceContext.Provider></PresentationContext.Provider></QueryClientProvider></MemoryRouter>);
    expect(api.reviewQueue).not.toHaveBeenCalled(); expect(api.reviewRuns).not.toHaveBeenCalled();
    return html;
  } finally { client.clear(); }
}
const queue = (items: ReviewPullSummary[]): ReviewQueue => ({ items, has_more: false, configured: true, project_id: 'project', snapshot_id: 'snapshot', revision: 1, source: 'stored', observed_at: '2026-10-19T00:00:00Z', recorded_at: '2026-10-19T00:00:00Z' });

describe('truthful stored review presentation', () => {
  it('computes signed distance only when the first page establishes the actual head', () => {
    const items = [pull(1, 'merged'), pull(2, 'closed'), pull(3), pull(4)];
    expect(reviewQueueDistances(items, true)).toEqual([-2, -1, 1, 2]);
    expect(reviewQueueDistances([pull(1, 'unknown'), pull(2)], true)).toEqual([undefined, undefined]);
    expect(reviewQueueDistances(items, false)).toEqual([undefined, undefined, undefined, undefined]);
    expect(reviewQueueDistances([pull(1, 'merged')], true)).toEqual([undefined]);
    expect(formatQueueDistance(-1)).toBe('−01'); expect(formatQueueDistance(1)).toBe('+01');
  });
  it('preserves order and repeats explicit noncontiguous headings without inventing unlabelled groups', () => {
    const items = [pull(4), pull(1), pull(3), pull(2)];
    const groups = [{ title: 'Foundation', pull_numbers: [1, 4, 2] }];
    const sections = groupReviewQueue(items, groups);
    expect(sections.map(section => section.title)).toEqual(['Foundation', undefined, 'Foundation']);
    expect(sections.flatMap(section => section.rows.map(row => row.pull.number))).toEqual([4, 1, 3, 2]);
    expect(groupReviewQueue(items, []).map(section => section.title)).toEqual([undefined]);
  });
  it('names only actually missing metadata and does not fabricate a missing head', () => {
    const incomplete = { ...pull(1), complete: false, base_sha: '', ci_state: 'unknown' as const };
    expect(missingPullFacts(incomplete)).toEqual(['Base commit was not captured.', 'CI status is unknown.']);
    const html = markup(<ReviewPullFacts pull={incomplete} />);
    expect(html).toContain('<summary>Missing PR details</summary>');
    expect(html).toContain('Base commit was not captured.'); expect(html).not.toContain('Head commit was not captured.');
    expect(html).toContain('CI: Unknown'); expect(html).toContain('Mergeability: Mergeable');
    expect(markup(<ReviewPullFacts pull={pull(1, 'merged')} />)).toContain('review-state-merged');
    expect(markup(<ReviewPullFacts pull={pull(1, 'merged')} />)).not.toContain('retained history');
  });
  it('renders retained merged rails, explicit groups and history arrows without automatic requests', () => {
    const presentation: Presentation = { project_id: 'project', revision: 1, name: 'Project', brand_name: 'FireController', tagline: '', headings: {}, theme: {}, updated_at: '', review_groups: [{ title: 'Foundation <literal>', pull_numbers: [1, 3] }] };
    const html = markup(<ReviewQueueView embedded />, queue([pull(1, 'merged'), pull(2), pull(3)]), presentation);
    expect(html).toContain('review-row-merged'); expect(html).toContain('Queue distance −01'); expect(html).toContain('Queue distance +01: first open PR');
    expect(html.match(/Foundation &lt;literal&gt;/gu)).toHaveLength(2);
    expect(html).toContain('href="https://github.com/example/project/pull/2"');
    expect(html).toContain('aria-label="Review details for PR #2"'); expect(html).toContain('review-link-arrow');
    expect(html).toContain('Merged PRs remain in the queue.'); expect(html).not.toContain('two days');
    expect(html).not.toContain('Latest attempt:'); expect(html).not.toContain('Incomplete PR observation');
  });
  it('omits completed attempt prose and shows a concise retained rate-limit signal only with evidence', () => {
    const attempt: ReviewAttemptSummary = { source_attempt_id: 'attempt', channel: 'cli', head_sha: 'a'.repeat(40), state: 'completed', started_at: '', finished_at: '', duration_seconds: 1, source_run_id: '', trigger_id: '', provider_review_id: '' };
    const props = { number: 1, snapshot: 'snapshot', head: 'a'.repeat(40), attempts: [attempt], expected: 0 };
    const completed = markup(<ReviewRunsView {...props} />);
    expect(completed).not.toContain('Latest attempt:'); expect(completed).not.toContain('review-attempt-state');
    expect(markup(<ReviewRunsView {...props} attempts={[{ ...attempt, state: 'rate_limited' }]} />)).toContain('Latest retained cli attempt: Cooldown');
    expect(markup(<ReviewRunsView {...props} attempts={[]} />)).not.toContain('review-attempt-state');
  });
  it('renders full finding Markdown safely while leaving titles and commit IDs literal', () => {
    const finding: ReviewFinding = { source_finding_id: 'id', title: '**Literal title**', detail: 'Useful **detail**\n\n- A step\n\n[unsafe](javascript:alert(1)) ![image](https://outside.invalid/image.png)', severity: 'low', disposition: 'accepted', decision_reason: 'Keep *context*.', actor: '', decided_at: '', fix_sha: '', proof_note: '`tested` with **evidence**', resolved_at: '' };
    const html = markup(<ReviewFindingView finding={finding} />);
    expect(html).toContain('<h3>**Literal title**</h3>'); expect(html).toContain('<strong>detail</strong>'); expect(html).toContain('<li>A step</li>'); expect(html).toContain('<em>context</em>'); expect(html).toContain('<code>tested</code>');
    expect(html).not.toContain('<img'); expect(html).not.toContain('href="javascript:');
  });
});
