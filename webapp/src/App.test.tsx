import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { CapturedComparisonRefusal, CapturedComparisonResult } from './App';

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
vi.mock('@tanstack/react-query', async importOriginal => ({ ...await importOriginal<object>(), useQuery: () => ({ data: assessmentContext.data, isPending: false }) }));
vi.mock('./workspace', async importOriginal => ({ ...await importOriginal<object>(), useWorkspace: () => ({ session: {}, key: ['test'] }), ReadState: ({ children }: { children: React.ReactNode }) => <>{children}</> }));
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
