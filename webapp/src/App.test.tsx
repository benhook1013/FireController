import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter, Route, Routes } from 'react-router';
import { CapturedComparisonRefusal, CapturedComparisonResult, ReviewPullView, ReviewQueueView } from './App';
import { useReviewProgress } from './review-progress-view';

describe('captured comparison refusal presentation', () => {
  it('explains the repository binding refusal without suggesting a refresh', () => {
    const html = renderToStaticMarkup(<CapturedComparisonRefusal code="review_held" />);
    expect(html).toContain('role="alert"');
    expect(html).toContain('project repository no longer matches this captured comparison');
    expect(html).toContain('administrator to verify the repository binding');
    expect(html).not.toMatch(/refresh|sufficient|merge.ready|PRIVATE_|<script>/iu);
  });
  it('explains a retained-history capacity refusal without suggesting a refresh', () => {
    const html = renderToStaticMarkup(<CapturedComparisonRefusal code="review_capacity" />);
    expect(html).toContain('role="alert"');
    expect(html).toContain('supported review-history capacity');
    expect(html).toContain('operator to address the retained history limit');
    expect(html).not.toMatch(/refresh|sufficient|merge.ready|PRIVATE_|<script>/iu);
  });
});


const assessmentContext = vi.hoisted(() => ({ data: {} as Record<string, unknown> }));
const progressHookContext = vi.hoisted(() => ({ calls: [] as unknown[], workspace: null as unknown, query: null as null | ((options: { queryKey: unknown[] }) => unknown) }));
vi.mock('@tanstack/react-query', async importOriginal => ({ ...await importOriginal<object>(), useQuery: (options: unknown) => { progressHookContext.calls.push(options); return progressHookContext.query ? progressHookContext.query(options as { queryKey: unknown[] }) : { data: assessmentContext.data, isPending: false }; } }));
vi.mock('./workspace', async importOriginal => ({ ...await importOriginal<object>(), useWorkspace: () => progressHookContext.workspace ?? ({ session: {}, key: ['test'] }), ReadState: ({ children, error }: { children: React.ReactNode; error?: unknown }) => error ? <div role="alert">Progress refresh failed</div> : <>{children}</> }));
describe('carried captured assessment presentation', () => {
  it.each([false, true])('shows carried holds and configured round completion %s', complete => {
    assessmentContext.data = { state: 'held', source_obligations_pending: 2, incoming_routes_pending: 1, policy_revision: 1, anchor: { pull_number: 7 }, channels: [{ channel: 'hosted', covered: true, pending: false, rounds_configured: true, rounds_complete: complete }] };
    const html = renderToStaticMarkup(<CapturedComparisonResult evidenceId="test" />);
    expect(html).toContain('2 carried source obligation(s) remain pending.');
    expect(html).toContain('1 incoming routed finding obligation(s) remain pending.');
    expect(html).toContain(`<dt>Required rounds complete</dt><dd>${complete ? 'Yes' : 'No'}</dd>`);
  });
  it('preserves unconfigured status alongside carried obligations', () => {
    assessmentContext.data = { state: 'unconfigured', source_obligations_pending: 1, policy_revision: 0, anchor: { pull_number: 7 }, channels: [] };
    const html = renderToStaticMarkup(<CapturedComparisonResult evidenceId="test" />);
    expect(html).toContain('Project review requirements are unconfigured.');
    expect(html).toContain('1 carried source obligation(s) remain pending.');
    expect(html).not.toContain('Required rounds complete');
  });
});

function ReviewProgressProbe() { useReviewProgress([17, 4, 17]); return null; }
describe('review progress App query wiring', () => {
  it('creates one current batched query and calls the scoped API once for the visible pull set', async () => {
    const read = vi.fn().mockResolvedValue({ items: [] });
    progressHookContext.calls = [];
    progressHookContext.workspace = { session: { api: { reviewProgress: read }, controller: new AbortController() }, key: ['projects', 'project_one'], fail: vi.fn() };
    renderToStaticMarkup(<ReviewProgressProbe />);
    const options = progressHookContext.calls.find(value => Array.isArray((value as { queryKey?: unknown[] }).queryKey) && (value as { queryKey: unknown[] }).queryKey.includes('round-progress')) as { queryKey: unknown[]; queryFn: (context: { signal: AbortSignal }) => Promise<unknown> };
    expect(progressHookContext.calls.filter(value => Array.isArray((value as { queryKey?: unknown[] }).queryKey) && (value as { queryKey: unknown[] }).queryKey.includes('round-progress'))).toHaveLength(1);
    expect(options.queryKey).toEqual(['projects', 'project_one', 'reviews', 'current', 'round-progress', '4,17']);
    await options.queryFn({ signal: new AbortController().signal });
    expect(read).toHaveBeenCalledTimes(1);
    expect(read).toHaveBeenCalledWith([4, 17], expect.any(AbortSignal));
    progressHookContext.workspace = null;
  });
});


describe('current review progress after a refresh error', () => {
  it.each(['queue', 'pull'])('hides retained progress in the %s view until a successful read', view => {
    const pull = { number: 7, title: 'Retained PR', state: 'open', ci_state: 'unknown', mergeability: 'unknown', complete: true, run_count: 0, attempt_count: 0, head_sha: 'a'.repeat(40), base_sha: 'b'.repeat(40), base_ref: 'main', latest_attempts: [] };
    const snapshot = { snapshot_id: 'snapshot_one', source: 'fixture', observed_at: '2026-11-10T12:00:00Z', recorded_at: '2026-11-10T12:00:00Z', revision: 1 };
    const progress = { items: [{ pull_number: 7, channels: [{ channel: 'hosted', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 3, taper: 1, used: 0, dry: 0, tapered: false, pending: 0, wait: { paused: false, queue_ahead_pull_number: 4 } }] }] };
    let error: Error | null = null;
    progressHookContext.workspace = { session: { project: { repository: 'example/repository' } }, key: ['projects', 'project_one'], home: '/projects/project_one', fail: vi.fn() };
    progressHookContext.query = ({ queryKey }) => {
      if (queryKey.includes('round-progress')) return { data: progress, error, isPending: false };
      if (queryKey.includes('queue')) return { data: { ...snapshot, configured: true, items: [pull], has_more: false } };
      if (queryKey.includes('runs') || queryKey.includes('attempts')) return { data: undefined, isPending: false };
      return { data: { ...snapshot, pull } };
    };
    const render = () => renderToStaticMarkup(<MemoryRouter initialEntries={['/projects/project_one/reviews/pulls/7']}><Routes><Route path="/projects/:projectId/reviews/pulls/:number" element={view === 'queue' ? <ReviewQueueView /> : <ReviewPullView />} /></Routes></MemoryRouter>);
    try {
      for (const failed of [false, true, false]) {
        error = failed ? new Error('refresh failed') : null;
        const html = render();
        expect(html).toContain('Retained PR');
        expect(html.includes('Hosted current round progress')).toBe(!failed);
        expect(html.includes('Up to 3 more reviews')).toBe(!failed);
        expect(html.includes('Behind #4')).toBe(!failed);
        expect(html.includes('Progress refresh failed')).toBe(failed);
      }
    } finally {
      progressHookContext.query = null;
      progressHookContext.workspace = null;
    }
  });
});
