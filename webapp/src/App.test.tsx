import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { CapturedComparisonRefusal, CapturedComparisonResult } from './App';
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
const progressHookContext = vi.hoisted(() => ({ calls: [] as unknown[], workspace: null as unknown }));
vi.mock('@tanstack/react-query', async importOriginal => ({ ...await importOriginal<object>(), useQuery: (options: unknown) => { progressHookContext.calls.push(options); return { data: assessmentContext.data, isPending: false }; } }));
vi.mock('./workspace', async importOriginal => ({ ...await importOriginal<object>(), useWorkspace: () => progressHookContext.workspace ?? ({ session: {}, key: ['test'] }), ReadState: ({ children }: { children: React.ReactNode }) => <>{children}</> }));
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
