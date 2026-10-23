import type { ReactNode } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { describe, expect, it, vi } from 'vitest';
import { ApiError } from './api';
import { WorkspaceContext, type Session } from './workspace';
import { CarriedObligationContent, ReviewObligations, type AdoptionObligationDetail, type AdoptionObligationPage } from './review-obligations';

const key = ['test', 'connection_one', 'instance_one', 'project_one'];
const listKey = [...key, 'reviews', 'current', 'adoption-obligations', 3251, ''];
const item = { id: 'obligation_one', namespace: 'sealed-source', source_key: 'finding-1', kind: 'finding', status: 'open', revision: 1, affected_pull_numbers: [3251] } as const;
const detail: AdoptionObligationDetail = {
  id: item.id, project_id: 'project_one', source: { namespace: item.namespace, repository_id: 17, snapshot_sha256: 'a'.repeat(64) }, source_revision: 1,
  original: { source_key: item.source_key, kind: 'finding', affected_pull_numbers: [3251], summary: '**Original correction**\n\n<script>doNotExecute()</script>\n\n[Unsafe](javascript:alert(1))', evidence: ['source:run-1/finding-1', '<img src=x onerror=alert(1)>'], original_status: 'pending' },
  status: 'open', revision: 1, actor_id: 'principal_one', worker_id: 'worker_one', created_at: '2026-10-16T01:00:00Z', resolution: null,
};

function fixture(page?: AdoptionObligationPage) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (page) client.setQueryData(listKey, page);
  const api = { reviewObligations: vi.fn(), reviewObligation: vi.fn() };
  const session = { api, controller: new AbortController() } as unknown as Session;
  const render = (content: ReactNode = <ReviewObligations number={3251} />) => renderToStaticMarkup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project_one', selectWorker: () => {}, fail: () => {}, ready: true }}>{content}</WorkspaceContext.Provider></QueryClientProvider>);
  return { client, api, render };
}

describe('private carried review work', () => {
  it('shows carried findings and unsettled results separately without eagerly expanding private evidence', () => {
    const f = fixture({ items: [{ ...item, affected_pull_numbers: [3251] }, { ...item, id: 'obligation_result', source_key: 'result-1', kind: 'result', affected_pull_numbers: [3251] }] });
    try {
      const html = f.render();
      expect(html).toContain('2 open items');
      expect(html).toContain('Carried review work');
      expect(html).toContain('hold new reviews until resolved');
      expect(html).toContain('Check the original evidence against current code');
      expect(html).toContain('Read carried finding finding-1');
      expect(html).toContain('Read carried unsettled result result-1');
      expect(html).toContain('<details><summary');
      expect(html).not.toMatch(/Original correction|doNotExecute|source:run-1/);
      expect(f.api.reviewObligation).not.toHaveBeenCalled();
    } finally { f.client.clear(); }
  });

  it('uses page count honestly and exposes bounded pagination when more items remain', () => {
    const f = fixture({ items: [{ ...item, affected_pull_numbers: [3251] }], next_cursor: 'cursor_next' });
    try {
      const html = f.render();
      expect(html).toContain('1 open item shown');
      expect(html).toContain('aria-label="Previous carried obligations" disabled=""');
      expect(html).toContain('aria-label="Next carried obligations"');
      expect(html).not.toContain('aria-label="Next carried obligations" disabled');
    } finally { f.client.clear(); }
  });

  it('distinguishes checked-empty from loading and failed reads, even with cached rows', () => {
    const empty = fixture({ items: [] });
    const loading = fixture();
    const failed = fixture({ items: [{ ...item, affected_pull_numbers: [3251] }] });
    try {
      expect(empty.render()).toContain('No open carried review work for this PR.');
      const pending = loading.render();
      expect(pending).toContain('Loading carried review work');
      expect(pending).not.toContain('No open carried');
      failed.client.getQueryCache().find({ queryKey: listKey })!.setState({ status: 'error', error: new ApiError('connection_failed', 'PRIVATE_REMOTE_FAILURE', 0) });
      const error = failed.render();
      expect(error).toContain('role="alert"');
      expect(error).toContain('Carried review work could not be checked.');
      expect(error).toContain('Retry carried work');
      expect(error).not.toMatch(/No open carried|0 open|finding-1|PRIVATE_REMOTE_FAILURE/);
    } finally { empty.client.clear(); loading.client.clear(); failed.client.clear(); }
  });

  it('renders selected original Markdown and evidence safely without inventing native review credit', () => {
    const f = fixture();
    try {
      const html = f.render(<CarriedObligationContent value={detail} />);
      expect(html).toContain('<strong>Original correction</strong>');
      expect(html).toContain('Open carried work');
      expect(html).toContain('originally pending');
      expect(html).toContain('source:run-1/finding-1');
      expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;');
      expect(html).toContain('sealed-source');
      expect(html).toContain('#3251');
      expect(html).not.toMatch(/<script|<img|href="javascript:|Captured comparison|Coverage confirmed|Resolved after cutover/);
    } finally { f.client.clear(); }
  });

  it('keeps original pending evidence distinct from later native resolution', () => {
    const f = fixture();
    try {
      const html = f.render(<CarriedObligationContent value={{ ...detail, status: 'resolved', revision: 2, resolution: { revision: 2, reason: '**Verified after cutover**', evidence: ['commit:verified-current-head'], actor_id: 'principal_two', worker_id: 'worker_two', resolved_at: '2026-10-23T01:00:00Z' } }} />);
      expect(html).toContain('Resolved after cutover');
      expect(html).toContain('originally pending');
      expect(html).toContain('<strong>Original correction</strong>');
      expect(html).toContain('<strong>Verified after cutover</strong>');
      expect(html).toContain('source:run-1/finding-1');
      expect(html).toContain('commit:verified-current-head');
    } finally { f.client.clear(); }
  });
});
