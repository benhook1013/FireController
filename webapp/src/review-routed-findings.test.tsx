import type { ReactNode } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api, ApiError } from './api';
import { refreshProjectMetadata } from './project-metadata';
import { WorkspaceContext, type Session } from './workspace';
import { CarriedObligationContent, type AdoptionObligationDetail } from './review-obligations';
import { NativeRoutedFindingContent, ReviewRoutedFindingDetails, ReviewRoutedFindings, type ReviewRoutedFindingDetail, type ReviewRoutedFindingItem, type ReviewRoutedFindingsPage } from './review-routed-findings';

const key = ['routed-test', 'connection', 'instance_one', 'project_one'];
const routeID = `review_route_${'a'.repeat(32)}`;
const evidenceID = `review_evidence_${'b'.repeat(32)}`;
const stamp = '2026-11-01T01:00:00Z';
const item = (changes: Partial<ReviewRoutedFindingItem> = {}): ReviewRoutedFindingItem => ({ id: routeID, kind: 'native', state: 'open', source_pull_numbers: [27], target_pull_number: 28, target_state: 'open', title: 'Preserve original evidence', revision: 1, created_at: stamp, source_evidence_id: evidenceID, ...changes });
const page = (number?: number, items: ReviewRoutedFindingItem[] = []): ReviewRoutedFindingsPage => ({ project_id: 'project_one', view: number === undefined ? 'attention' : 'pull', pull_number: number ?? 0, counts: { open: 0, fixed: 0, rejected: 0, resolved: 0 }, items, has_more: false });
const listKey = (number?: number, cursor = '') => [...key, 'reviews', 'current', 'routed-findings', number ?? 'attention', cursor];
const detailKey = (value = item()) => [...key, 'reviews', 'current', 'routed-findings', 'detail', value.kind, value.id, value.revision];
const native: ReviewRoutedFindingDetail = {
  route: { id: routeID, project_id: 'project_one', repository_id: 17, source_evidence_id: evidenceID, finding: 0, source_pull_number: 27, source_head_sha: 'c'.repeat(40), target_pull_number: 28, state: 'active', revision: 1, disposition: 'accepted', routing_reason: '**Another PR owns this correction.**', routing_actor_id: 'principal_source', reason: '**Verified** receiving decision.', actor_id: 'principal_receiver', decided_at: stamp, fix_sha: 'd'.repeat(40), proof_note: 'The focused **regression** passed.', fixed_at: stamp, created_at: stamp, updated_at: stamp },
  source_anchor: { repository_id: 17, repository: 'owner/project', pull_number: 27, candidate_sha: 'c'.repeat(40), base_sha: 'e'.repeat(40), parent_sha: 'e'.repeat(40), parent_pull_number: 0, base_ref: 'main', merge_base_sha: 'e'.repeat(40), patch_sha256: 'f'.repeat(64), observed_at: stamp },
  source_finding: { finding: 0, title: 'Preserve original evidence', detail: '<picture><img src="https://outside.invalid/pixel"></picture>\n\nThe decoder loses **original bytes**.\n\n<script>executeMe()</script>\n\n[unsafe](javascript:alert(1))', severity: 'high', path: 'source/file.go', start_line: 10, end_line: 12 },
};
const carried: AdoptionObligationDetail = {
  id: `adoption_obligation_${'c'.repeat(32)}`, project_id: 'project_one', source: { namespace: 'original-controller', repository_id: 17, snapshot_sha256: 'd'.repeat(64) }, source_revision: 1,
  original: { source_key: 'old-route', kind: 'route', affected_pull_numbers: [28], summary: '**Carried correction**', evidence: ['original:route'], original_status: 'pending' },
  status: 'resolved', revision: 2, actor_id: 'principal', worker_id: 'worker', created_at: stamp,
  resolution: { revision: 2, reason: '**Resolved with retained proof**', evidence: ['commit:original-fix'], actor_id: 'principal', worker_id: 'worker', resolved_at: stamp },
};
const clients: QueryClient[] = [];
afterEach(() => { for (const client of clients.splice(0)) client.clear(); vi.unstubAllGlobals(); });

function fixture(value?: ReviewRoutedFindingsPage, number?: number, apiOverride?: Api) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); clients.push(client);
  if (value) client.setQueryData(listKey(number), value);
  const api = { reviewRoutedFindings: vi.fn(async () => value!), reviewRoute: vi.fn(async (_id: string, _signal?: AbortSignal) => native), reviewObligation: vi.fn(async (_id: string, _signal?: AbortSignal) => carried) };
  const fail = vi.fn();
  const session = { project: { id: 'project_one', repository: 'owner/project' }, controller: new AbortController(), api: apiOverride ?? api } as unknown as Session;
  const render = (node: ReactNode = <ReviewRoutedFindings number={number} />) => renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project_one', ready: true, fail, selectWorker: () => {} }}>{node}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
  return { client, api, session, fail, render };
}

describe('current routed findings', () => {
  it('shows incoming and outgoing routes together, counts the full selection and leaves evidence unopened', () => {
    const incoming = item({ id: `review_route_${'d'.repeat(32)}`, source_pull_numbers: [26], target_pull_number: 27, title: 'Incoming correction', state: 'fixed' });
    const outgoing = item({ title: 'Outgoing correction' });
    const f = fixture({ ...page(27, [incoming, outgoing]), counts: { open: 41, fixed: 8, rejected: 3, resolved: 2 }, has_more: true, cursor: 'next_page' }, 27);
    const html = f.render();
    for (const text of ['Routed Findings', 'Incoming correction', 'Outgoing correction', 'Incoming', 'Outgoing', 'Open: <strong>41</strong>', 'Fixed: <strong>8</strong>', 'Rejected: <strong>3</strong>', 'Resolved: <strong>2</strong>']) expect(html).toContain(text);
    for (const number of [26, 27, 28]) expect(html).toContain(`href="/projects/project_one/reviews/pulls/${number}"`);
    expect(html).toContain('aria-label="Next routed findings"');
    expect(html).not.toContain('aria-label="Next routed findings" disabled');
    expect(html).not.toMatch(/snapshot=|review_route_|review_evidence_|original bytes|<pre/);
    expect(f.api.reviewRoute).not.toHaveBeenCalled(); expect(f.api.reviewObligation).not.toHaveBeenCalled();
  });

  it('keeps outstanding routes visible when their receiving PR closed or merged and distinguishes unknown from unassigned', () => {
    const items = [item({ target_state: 'closed', title: 'Closed receiving PR' }), item({ id: 'merged', target_state: 'merged', title: 'Merged receiving PR' }), item({ id: 'unknown', kind: 'carried', source_pull_numbers: [], target_pull_number: undefined, target_state: 'unknown', title: 'Unknown origin' }), item({ id: 'unassigned', kind: 'carried', source_pull_numbers: [], target_pull_number: undefined, target_state: 'unassigned', title: 'Awaiting assignment' })];
    const f = fixture({ ...page(undefined, items), counts: { open: 4, fixed: 0, rejected: 0, resolved: 0 } });
    const html = f.render();
    expect(html).toContain('Receiving PR is closed; this route remains open.');
    expect(html).toContain('Receiving PR is merged; this route remains open.');
    expect(html).toContain('Source: PR unknown');
    expect(html).toContain('Receiving: Unknown receiving PR');
    expect(html).toContain('Receiving: Unassigned');
    expect(html).toContain('No receiving PR is assigned.');
    expect(html).not.toMatch(/No routes|Resolved after cutover|automatic|ready to merge/i);
  });

  it('hides only checked-empty attention and does not flash loading space or claim an empty PR before its read', () => {
    const empty = fixture(page()); expect(empty.render()).toBe('');
    const loading = fixture(); expect(loading.render()).toBe('');
    const pr = fixture(page(27), 27); const html = pr.render();
    expect(html).toContain('No routes recorded.'); expect(html).toContain('Open: <strong>0</strong>'); expect(html).not.toContain('Resolved:');
    const pendingPR = fixture(undefined, 27); const pending = pendingPR.render();
    expect(pending).toContain('Checking routed findings'); expect(pending).not.toMatch(/No routes|Open:|skeleton/);
  });

  it('shows compact retry errors and suppresses stale work/counts and private error text on read failures', () => {
    for (const error of [new ApiError('connection_failed', 'PRIVATE response', 0), new ApiError('forbidden', 'PRIVATE revoked data', 403)]) {
      const f = fixture({ ...page(undefined, [item()]), counts: { open: 1, fixed: 0, rejected: 0, resolved: 0 } });
      f.client.getQueryCache().find({ queryKey: listKey() })!.setState({ status: 'error', error });
      const html = f.render();
      expect(html).toContain('role="alert"'); expect(html).toContain('Routed findings could not be checked.');
      expect(html).not.toMatch(/PRIVATE|Preserve original evidence|Open:|No routes/);
      expect(html.includes('Retry routed findings')).toBe(error.status !== 403);
    }
  });

  it('loads one bounded current page through the actual API with scoped query and no empty cursor or detail fanout', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    const api = new Api('synthetic-private-credential', 'project_one');
    const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } });
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : json({ id: 'project_one', name: 'Project', github_repository_id: 17, repository: 'owner/project' })));
    try {
      await api.connect();
      for (const number of [undefined, 27]) {
        const value = page(number);
        const fetch = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => json(value)); vi.stubGlobal('fetch', fetch);
        const f = fixture(undefined, number, api); f.render();
        const queryKey = listKey(number); const query = f.client.getQueryCache().find({ queryKey })!;
        expect(await f.client.fetchQuery({ ...query.options, queryKey })).toEqual(value);
        expect(fetch).toHaveBeenCalledTimes(1);
        const url = new URL(String(fetch.mock.calls[0][0]));
        expect(url.pathname).toBe('/api/v1/projects/project_one/reviews/routed-findings');
        expect(url.searchParams.get('limit')).toBe('20'); expect(url.searchParams.has('cursor')).toBe(false);
        expect(url.searchParams.get('pull_number')).toBe(number === undefined ? null : '27');
        expect(url.searchParams.get('view')).toBe(number === undefined ? 'attention' : null);
        expect(fetch.mock.calls[0][1]?.method ?? 'GET').toBe('GET');
      }
    } finally { api.close(); }
  });

  it('reads selected native and carried details only on demand, caches reopens and keys revisions independently', async () => {
    const f = fixture();
    f.render(<ReviewRoutedFindingDetails item={item()} />);
    const queryKey = detailKey(); const query = f.client.getQueryCache().find({ queryKey })!;
    expect(query.options.gcTime).toBe(5 * 60 * 1000);
    await f.client.fetchQuery({ ...query.options, queryKey });
    const html = f.render(<ReviewRoutedFindingDetails item={item()} />);
    expect(html).toContain('Another PR owns this correction.');
    expect(html).toContain('<strong>original bytes</strong>');
    expect(html).toContain('<strong>Verified</strong> receiving decision.'); // canonical decision prose retains safe Markdown.
    expect(f.api.reviewRoute).toHaveBeenCalledTimes(1);
    expect(f.api.reviewRoute.mock.calls[0][0]).toBe(routeID);
    expect(f.api.reviewRoute.mock.calls[0][1]).toBeInstanceOf(AbortSignal);
    await f.client.fetchQuery({ ...query.options, queryKey }); expect(f.api.reviewRoute).toHaveBeenCalledTimes(1);
    const next = item({ revision: 2 }); f.api.reviewRoute.mockResolvedValueOnce({ ...native, route: { ...native.route, revision: 2 } });
    f.render(<ReviewRoutedFindingDetails item={next} />);
    const nextKey = detailKey(next); const nextQuery = f.client.getQueryCache().find({ queryKey: nextKey })!;
    await f.client.fetchQuery({ ...nextQuery.options, queryKey: nextKey }); expect(f.api.reviewRoute).toHaveBeenCalledTimes(2);
    const carriedItem = item({ id: carried.id, kind: 'carried', revision: 2, source_pull_numbers: [], target_pull_number: undefined, target_state: 'unknown' });
    f.render(<ReviewRoutedFindingDetails item={carriedItem} />);
    const carriedKey = detailKey(carriedItem); const carriedQuery = f.client.getQueryCache().find({ queryKey: carriedKey })!;
    await f.client.fetchQuery({ ...carriedQuery.options, queryKey: carriedKey });
    expect(f.render(<ReviewRoutedFindingDetails item={carriedItem} />)).toContain('Resolved after cutover');
    expect(f.api.reviewObligation).toHaveBeenCalledTimes(1);
  });

  it('invalidates cached list and reopened detail on receiving updates without fetching closed details or another project', async () => {
    const f = fixture({ ...page(27, [item()]), counts: { open: 1, fixed: 0, rejected: 0, resolved: 0 } }, 27);
    f.client.setQueryData(detailKey(), { kind: 'native', value: native });
    const otherKey = ['other-project', ...listKey(27).slice(1)]; f.client.setQueryData(otherKey, page(27));
    await refreshProjectMetadata(f.client, key, { items: [{ event_type: 'review.route.fix_recorded', resource_id: routeID, revision: 2, event_cursor: 'event-one' }], event_cursor: 'event-one', has_more: false }, new AbortController().signal);
    expect(f.client.getQueryState(listKey(27))?.isInvalidated).toBe(true);
    expect(f.client.getQueryState(detailKey())?.isInvalidated).toBe(true);
    expect(f.client.getQueryState(otherKey)?.isInvalidated).toBe(false);
    expect(f.api.reviewRoute).not.toHaveBeenCalled(); expect(f.api.reviewObligation).not.toHaveBeenCalled();
  });

  it('preserves original finding and fix proof through the canonical safe renderer without remote media or executable markup', () => {
    const f = fixture(); const html = f.render(<NativeRoutedFindingContent value={native} />);
    expect(html).toContain('<strong>Another PR owns this correction.</strong>');
    expect(html).toContain('<strong>original bytes</strong>');
    expect(html).toContain('<strong>Verified</strong>'); expect(html).toContain('<strong>regression</strong>');
    expect(html).toContain(`href="https://github.com/owner/project/commit/${native.route.fix_sha}"`);
    expect(html).toContain('source/file.go:10–12');
    expect(html).not.toMatch(/<script|<picture|<img|href="javascript:/);
    expect(f.render(<CarriedObligationContent value={carried} />)).toContain('Resolved with retained proof');
  });

  it('escapes hostile summaries and keeps square outer/rounded inner surfaces and mobile wrapping rules', async () => {
    const f = fixture({ ...page(27, [item({ title: '<img src=x onerror=run()> ' + 'long'.repeat(80) })]), counts: { open: 1, fixed: 0, rejected: 0, resolved: 0 } }, 27);
    const html = f.render(); expect(html).toContain('&lt;img'); expect(html).not.toContain('<img');
    const fs = await vi.importActual<{ readFileSync(path: URL, encoding: 'utf8'): string }>('node:fs');
    const styles = fs.readFileSync(new URL('./review-routed-findings.css', import.meta.url), 'utf8');
    expect(styles).toMatch(/\.review-routed-findings\{[^}]*border-radius:0/);
    expect(styles).toMatch(/\.review-routed-item\{[^}]*border-radius:7px/);
    expect(styles).toContain('overflow-wrap:anywhere'); expect(styles).toContain('@media(max-width:650px)');
  });
});
