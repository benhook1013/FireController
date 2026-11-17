import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import type { ReviewRoutedFindingDetail, ReviewRoutedFindingItem, ReviewRoutedFindingsPage } from './review-routed-findings';

const project = { id: 'project_one', name: 'Routed findings', github_repository_id: 17, repository: 'owner/project' };
const routeID = `review_route_${'a'.repeat(32)}`;
const evidenceID = `review_evidence_${'b'.repeat(32)}`;
const at = '2026-11-01T01:00:00.000000000Z';
const clients: Api[] = [];
function json(value: unknown): Response { return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } }); }
async function connected(): Promise<Api> {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = new Api('synthetic-private-token', project.id); clients.push(api);
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : json(project)));
  await api.connect(); return api;
}
afterEach(() => { for (const api of clients.splice(0)) api.close(); vi.unstubAllGlobals(); });

function detail(): ReviewRoutedFindingDetail {
  return {
    route: { id: routeID, project_id: project.id, repository_id: 17, source_evidence_id: evidenceID, finding: 0, source_pull_number: 27, source_head_sha: 'c'.repeat(40), target_pull_number: 28, state: 'active', revision: 1, disposition: 'unadjudicated', routing_reason: 'Receiving PR owns this correction.', routing_actor_id: 'principal_source', reason: '', actor_id: '', decided_at: '', fix_sha: '', proof_note: '', fixed_at: '', created_at: at, updated_at: at },
    source_anchor: { repository_id: 17, repository: 'owner/project', pull_number: 27, candidate_sha: 'c'.repeat(40), base_sha: 'd'.repeat(40), parent_sha: 'd'.repeat(40), parent_pull_number: 0, base_ref: 'main', merge_base_sha: 'd'.repeat(40), patch_sha256: 'e'.repeat(64), observed_at: at },
    source_finding: { finding: 0, title: 'Original <literal> finding', detail: 'The original **finding** remains separate from receiving proof.', severity: 'high', path: 'source/file.go', start_line: 10, end_line: 12 },
  };
}
function item(): ReviewRoutedFindingItem { return { id: routeID, kind: 'native', state: 'open', source_pull_numbers: [27], target_pull_number: 28, target_state: 'open', title: 'Original finding', revision: 1, created_at: at, source_evidence_id: evidenceID }; }
function page(): ReviewRoutedFindingsPage { return { project_id: project.id, view: 'pull', pull_number: 27, counts: { open: 4, fixed: 2, rejected: 1, resolved: 0 }, items: [item()], has_more: true, cursor: 'signed_next_page' }; }

describe('routed findings API boundary', () => {
  it('decodes canonical current route detail without rewriting source or receiving proof and makes one scoped GET', async () => {
    const api = await connected();
    for (const state of ['unadjudicated', 'accepted', 'rejected', 'fixed'] as const) {
      const value = detail();
      if (state !== 'unadjudicated') Object.assign(value.route, { disposition: state === 'fixed' ? 'accepted' : state, revision: 2, reason: 'Explicit receiving decision', actor_id: 'principal_receiver', decided_at: at });
      if (state === 'fixed') Object.assign(value.route, { revision: 3, fix_sha: 'f'.repeat(40), proof_note: 'Focused **proof** preserved.', fixed_at: at });
      const fetch = vi.fn(async () => json(value)); vi.stubGlobal('fetch', fetch);
      expect(await api.reviewRoute(routeID)).toEqual(value);
      expect(fetch).toHaveBeenCalledTimes(1);
      const [url, init] = fetch.mock.calls[0] as unknown as [string, RequestInit];
      expect(new URL(url).pathname).toBe(`/api/v1/projects/project_one/reviews/routes/${routeID}`);
      expect(new URL(url).search).toBe('');
      expect(init).toMatchObject({ method: 'GET', cache: 'no-store', redirect: 'error' });
      expect(new Headers(init.headers).get('x-fc-instance')).toBe('instance_one');
      expect(new Headers(init.headers).has('idempotency-key')).toBe(false);
      expect(url).not.toContain('synthetic-private-token');
    }
  });

  it('refuses wrong project, selected route and original evidence/source bindings without retrying', async () => {
    const api = await connected();
    const changes: Array<(value: ReviewRoutedFindingDetail) => void> = [
      value => { value.route.project_id = 'project_other'; },
      value => { value.route.id = `review_route_${'f'.repeat(32)}`; },
      value => { value.route.source_evidence_id = 'not-evidence'; },
      value => { value.source_anchor.pull_number = 28; },
      value => { value.source_anchor.repository_id = 18; },
      value => { value.source_anchor.candidate_sha = 'f'.repeat(40); },
      value => { value.source_finding.finding = 1; },
      value => { value.route.target_pull_number = value.route.source_pull_number; },
    ];
    for (const change of changes) {
      const value = detail(); change(value);
      const fetch = vi.fn(async () => json(value)); vi.stubGlobal('fetch', fetch);
      await expect(api.reviewRoute(routeID)).rejects.toMatchObject({ code: 'invalid_response' });
      expect(fetch).toHaveBeenCalledTimes(1);
    }
    const fetch = vi.fn(); vi.stubGlobal('fetch', fetch);
    await expect(api.reviewRoute('../other-route')).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetch).not.toHaveBeenCalled();
  });

  it('retains full counts and an explicit selected page without draining its continuation or fetching details', async () => {
    const api = await connected(); const value = page();
    const fetch = vi.fn(async () => json(value)); vi.stubGlobal('fetch', fetch);
    expect(await api.reviewRoutedFindings({ pullNumber: 27, limit: 1, cursor: 'signed_previous_page' })).toEqual(value);
    expect(fetch).toHaveBeenCalledTimes(1);
    const [input, init] = fetch.mock.calls[0] as unknown as [string, RequestInit]; const url = new URL(input);
    expect(url.pathname).toBe('/api/v1/projects/project_one/reviews/routed-findings');
    expect([...url.searchParams.entries()].sort()).toEqual([['cursor', 'signed_previous_page'], ['limit', '1'], ['pull_number', '27']]);
    expect(init.method).toBe('GET');
  });

  it('refuses cross-project, wrong-selector, contradictory page counts and malformed native items', async () => {
    const api = await connected();
    const changes: Array<(value: ReviewRoutedFindingsPage) => void> = [
      value => { value.project_id = 'project_other'; },
      value => { value.pull_number = 28; },
      value => { value.items[0].source_pull_numbers = [29]; },
      value => { value.items[0].source_pull_numbers = [28]; },
      value => { value.counts.open = 0; },
      value => { value.items.push(value.items[0]); },
      value => { value.items[0].source_evidence_id = 'not-evidence'; },
      value => { value.has_more = true; delete value.cursor; },
    ];
    for (const change of changes) {
      const value = page(); change(value);
      const fetch = vi.fn(async () => json(value)); vi.stubGlobal('fetch', fetch);
      await expect(api.reviewRoutedFindings({ pullNumber: 27 })).rejects.toMatchObject({ code: 'invalid_response' });
      expect(fetch).toHaveBeenCalledTimes(1);
    }
  });

  it('keeps attention limited to unfinished routes and does not invent closed state for an unknown carried target', async () => {
    const api = await connected();
    const value: ReviewRoutedFindingsPage = {
      project_id: project.id, view: 'attention', pull_number: 0, counts: { open: 2, fixed: 0, rejected: 0, resolved: 0 }, has_more: false,
      items: [{ ...item(), target_state: 'closed' }, { id: `adoption_obligation_${'f'.repeat(32)}`, kind: 'carried', state: 'open', source_pull_numbers: [], target_state: 'unknown', title: 'Original carried route', revision: 1, created_at: at }],
    };
    const fetch = vi.fn(async () => json(value)); vi.stubGlobal('fetch', fetch);
    expect(await api.reviewRoutedFindings()).toEqual(value);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(new URL(String((fetch.mock.calls[0] as unknown as [string])[0])).searchParams.get('view')).toBe('attention');
    for (const invalid of [{ ...value, counts: { ...value.counts, fixed: 1 } }, { ...value, items: [{ ...item(), target_state: 'open' }] }]) {
      const fetch = vi.fn(async () => json(invalid)); vi.stubGlobal('fetch', fetch);
      await expect(api.reviewRoutedFindings()).rejects.toMatchObject({ code: 'invalid_response' });
      expect(fetch).toHaveBeenCalledTimes(1);
    }
  });
});

it('preserves explicit unassigned native detail and summary without manufacturing a target PR', async () => {
  const api = await connected(); const value = detail(); value.route.target_pull_number = 0;
  vi.stubGlobal('fetch', vi.fn(async () => json(value)));
  expect(await api.reviewRoute(routeID)).toEqual(value);
  const listing = page(); delete listing.items[0].target_pull_number; listing.items[0].target_state = 'unassigned';
  vi.stubGlobal('fetch', vi.fn(async () => json(listing)));
  expect(await api.reviewRoutedFindings({pullNumber:27})).toEqual(listing);
  for (const target of [undefined,null,-1]) {
    const invalid = {...value, route:{...value.route,target_pull_number:target}};
    vi.stubGlobal('fetch', vi.fn(async () => json(invalid)));
    await expect(api.reviewRoute(routeID)).rejects.toMatchObject({code:'invalid_response'});
  }
});
