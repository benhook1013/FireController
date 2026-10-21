import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ReviewQueueView, reviewQueueQuery } from './review-workspace';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewQueue } from './types';

function queueMarkup(configured = true, history = false) {
  const key = ['queue-technical'], api = { reviewQueue: vi.fn(), reviewRuns: vi.fn(), reviewFronts: vi.fn() };
  const session = { api, controller: new AbortController(), project: { id: 'project', repository: 'example/project' } } as unknown as Session;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queue: ReviewQueue = { items: [], configured, has_more: false, project_id: 'project', snapshot_id: 'snapshot<literal>', revision: 7, source: 'retained <source>', observed_at: '2026-10-21T10:00:00Z', recorded_at: '2026-10-21T11:00:00Z' };
  client.setQueryData(reviewQueueQuery(session, key, history ? 'merged' : 'recent').queryKey, queue);
  try {
    const html = renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project', ready: true, fail: () => {}, selectWorker: () => {} }}><ReviewQueueView history={history} /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
    expect(api.reviewQueue).not.toHaveBeenCalled(); expect(api.reviewRuns).not.toHaveBeenCalled(); expect(api.reviewFronts).not.toHaveBeenCalled();
    return html;
  } finally { client.clear(); }
}

describe('queue-only technical details control', () => {
  it.each([false, true])('keeps diagnostics in a closed heading control rather than a body row (history=%s)', history => {
    const html = queueMarkup(true, history);
    const control = html.indexOf('<details class="review-technical-details"'), body = html.indexOf('panel review-page-body');
    expect(control).toBeGreaterThan(0); expect(control).toBeLessThan(body);
    expect(html).toContain('<summary aria-label="Technical details" title="Technical details">ⓘ</summary>');
    expect(html).not.toContain('review-technical-details" open'); expect(html).not.toContain('review-freshness');
    expect(html).toContain('Source timestamp'); expect(html).toContain('individual PR facts can have different ages.');
    expect(html).toContain('Saved locally'); expect(html).toContain('2026-10-21T10:00:00Z'); expect(html).toContain('2026-10-21T11:00:00Z');
    expect(html).toContain('snapshot&lt;literal&gt;'); expect(html).toContain('retained &lt;source&gt;'); expect(html).toContain('revision 7');
    expect(html.slice(body)).not.toContain('Source timestamp'); expect(html.slice(body)).not.toContain('Saved locally');
    expect(html).toContain(history ? 'Back to review queue ↗' : 'Queue history ↗');
  });
  it('does not fabricate diagnostics when there is no configured queue', () => {
    const html = queueMarkup(false);
    expect(html).not.toContain('review-technical-details'); expect(html).toContain('No review queue yet');
  });
});
