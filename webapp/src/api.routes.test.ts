import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';

const project = { id: 'project_one', name: 'Synthetic routes', github_repository_id: 123, repository: 'owner/repository' };
const evidenceID = `review_evidence_${'a'.repeat(32)}`;
const runID = `review_run_${'b'.repeat(32)}`;
const snapshotID = `review_snapshot_${'c'.repeat(32)}`;
const at = '2026-10-15T12:00:00Z';
type Wire = Record<string, unknown>;

function json(value: unknown): Response {
  return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } });
}
async function connected(): Promise<Api> {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const client = new Api('synthetic-private-token', project.id);
  vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ instance_id: 'instance_one' })).mockResolvedValueOnce(json(project)));
  await client.connect();
  return client;
}
function receiver(): Wire {
  return { id: `review_route_${'d'.repeat(32)}`, target_pull_number: 4, revision: 1, state: 'active', disposition: 'unadjudicated', reason: '', actor: '', decided_at: '', fix_sha: '', proof_note: '', fixed_at: '' };
}
function routedDetail(): Wire {
  return {
    project_id: project.id, revision: 3, snapshot_id: snapshotID, source: 'native-executor', observed_at: at, recorded_at: at, pull_number: 2,
    run: { source: 'native-executor', id: runID, pull_number: 2, source_run_id: evidenceID, channel: 'hosted', head_sha: 'e'.repeat(40), reviewer: 'coderabbit', scope: 'admitted comparison', coverage_limits: [], outcome: 'completed', attributable: true, complete: true, started_at: at, finished_at: at, found: 1, accepted: 0, routed: 1, finalized: true, fixed: 0 },
    findings: [{ source_finding_id: `${evidenceID}:finding:0`, title: '<Literal finding>', detail: 'Exact original source detail.\nNo receiver rewrite.', severity: 'high', disposition: 'routed', decision_reason: 'Owned by receiving PR', actor: 'principal_source', decided_at: at, fix_sha: '', proof_note: '', resolved_at: '', route: receiver() }],
  };
}
function finding(detail: Wire): Wire { return (detail.findings as Wire[])[0]; }
function route(detail: Wire): Wire { return finding(detail).route as Wire; }
function run(detail: Wire): Wire { return detail.run as Wire; }
function assessment(): Wire {
  return { project_id: project.id, evidence_id: evidenceID, source: 'captured_comparison', policy_revision: 2, evaluated_at: at, state: 'sufficient', anchor: { repository_id: 123, repository: 'owner/repository', pull_number: 4, parent_pull_number: 0, candidate_sha: 'b'.repeat(40), base_sha: 'c'.repeat(40), parent_sha: 'c'.repeat(40), merge_base_sha: 'c'.repeat(40), patch_sha256: 'f'.repeat(64), base_ref: 'main', observed_at: at }, channels: [{ channel: 'hosted', covered: true, pending: false }] };
}

afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('native routed finding browser reads', () => {
  it('preserves source facts and separate pending, rejected and fixed receiver facts using one bound GET', async () => {
    const client = await connected();
    for (const disposition of ['unadjudicated', 'accepted', 'rejected', 'fixed']) {
      const value = routedDetail();
      if (disposition !== 'unadjudicated') Object.assign(route(value), { disposition: disposition === 'fixed' ? 'accepted' : disposition, reason: 'Receiving decision <literal>', actor: 'principal_receiver', decided_at: at, revision: 2 });
      if (disposition === 'fixed') Object.assign(route(value), { fix_sha: 'f'.repeat(40), proof_note: 'Focused receiving proof <script>literal</script>', fixed_at: at, revision: 3 });
      const fetchMock = vi.fn().mockResolvedValueOnce(json(value));
      vi.stubGlobal('fetch', fetchMock);
      const got = await client.reviewRun(runID, snapshotID);
      expect(got).toEqual(value);
      expect(got.run).toMatchObject({ found: 1, accepted: 0, routed: 1, fixed: 0 });
      expect(got.findings[0]).toMatchObject({ actor: 'principal_source', decision_reason: 'Owned by receiving PR', fix_sha: '', proof_note: '' });
      expect(fetchMock).toHaveBeenCalledTimes(1);
      const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
      expect(new URL(url).pathname).toBe(`/api/v1/projects/project_one/reviews/runs/${runID}`);
      expect(new URL(url).searchParams.get('snapshot')).toBe(snapshotID);
      expect(init).toMatchObject({ method: 'GET', cache: 'no-store', redirect: 'error' });
      expect(new Headers(init.headers).get('x-fc-instance')).toBe('instance_one');
      expect(url).not.toContain('synthetic-private-token');
    }
    client.close();
  });

  it('retains legacy imported routed history without inventing a native receiver', async () => {
    const client = await connected();
    const legacy = routedDetail();
    legacy.source = 'historical-import';
    run(legacy).source = 'historical-import';
    run(legacy).source_run_id = 'manual-historical-observation';
    delete finding(legacy).route;
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(legacy)));
    expect(await client.reviewRun(runID)).toEqual(legacy);
    // An imported as-of projection can explicitly retain a superseded route.
    finding(legacy).route = { ...receiver(), state: 'superseded' };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(legacy)));
    expect((await client.reviewRun(runID)).findings[0].route?.state).toBe('superseded');
    client.close();
  });

  it('requires actual string route enums even in imported historical receiver data', async () => {
    const client = await connected();
    for (const state of [['active'], ['superseded']]) {
      const value = routedDetail();
      run(value).source = 'historical-import';
      route(value).state = state;
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(value)));
      await expect(client.reviewRun(runID)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    client.close();
  });

  const malformed: Array<[string, (v: Wire) => void]> = [
    ['missing native route', v => { delete finding(v).route; }],
    ['null native route', v => { finding(v).route = null; }],
    ['superseded native route', v => { route(v).state = 'superseded'; }],
    ['self target', v => { route(v).target_pull_number = 2; }],
    ['different source PR', v => { run(v).pull_number = 4; }],
    ['different project', v => { v.project_id = 'project_two'; }],
    ['uppercase route id', v => { route(v).id = `review_route_${'A'.repeat(32)}`; }],
    ['invalid target', v => { route(v).target_pull_number = 2147483648; }],
    ['null target', v => { route(v).target_pull_number = null; }],
    ['string target', v => { route(v).target_pull_number = '4'; }],
    ['zero route revision', v => { route(v).revision = 0; }],
    ['null route revision', v => { route(v).revision = null; }],
    ['missing required receiver field', v => { delete route(v).fixed_at; }],
    ['unrecognized receiver field', v => { route(v).source_head_sha = 'e'.repeat(40); }],
    ['unknown receiver decision', v => { route(v).disposition = 'routed'; }],
    ['array receiver decision', v => { Object.assign(route(v), { disposition: ['accepted'], reason: 'Valid reason', actor: 'principal_receiver', decided_at: at }); }],
    ['null receiver decision', v => { route(v).disposition = null; }],
    ['undecided receiver with proof', v => { route(v).fix_sha = 'f'.repeat(40); }],
    ['decided receiver without reason', v => { Object.assign(route(v), { disposition: 'rejected', actor: 'principal_receiver', decided_at: at }); }],
    ['decided receiver without actor', v => { Object.assign(route(v), { disposition: 'accepted', reason: 'Valid reason', decided_at: at }); }],
    ['decided receiver without time', v => { Object.assign(route(v), { disposition: 'accepted', reason: 'Valid reason', actor: 'principal_receiver' }); }],
    ['receiver proof on rejected', v => { Object.assign(route(v), { disposition: 'rejected', reason: 'Valid reason', actor: 'principal_receiver', decided_at: at, fix_sha: 'f'.repeat(40), proof_note: 'Proof', fixed_at: at }); }],
    ['receiver fix without proof', v => { Object.assign(route(v), { disposition: 'accepted', reason: 'Valid reason', actor: 'principal_receiver', decided_at: at, fix_sha: 'f'.repeat(40), fixed_at: at }); }],
    ['receiver proof without SHA', v => { Object.assign(route(v), { disposition: 'accepted', reason: 'Valid reason', actor: 'principal_receiver', decided_at: at, proof_note: 'Proof', fixed_at: at }); }],
    ['uppercase nested fix', v => { Object.assign(route(v), { disposition: 'accepted', reason: 'Valid reason', actor: 'principal_receiver', decided_at: at, fix_sha: 'F'.repeat(40), proof_note: 'Proof', fixed_at: at }); }],
    ['oversized nested proof', v => { Object.assign(route(v), { disposition: 'accepted', reason: 'Valid reason', actor: 'principal_receiver', decided_at: at, fix_sha: 'f'.repeat(40), proof_note: 'é'.repeat(1025), fixed_at: at }); }],
    ['source proof on routed finding', v => { finding(v).fix_sha = 'f'.repeat(40); run(v).fixed = 1; }],
    ['receiver on non-routed source', v => { finding(v).disposition = 'rejected'; }],
  ];
  it.each(malformed)('rejects %s instead of rendering misleading native proof', async (_name, edit) => {
    const client = await connected();
    const value = routedDetail(); edit(value);
    const fetchMock = vi.fn().mockResolvedValueOnce(json(value)); vi.stubGlobal('fetch', fetchMock);
    await expect(client.reviewRun(runID, snapshotID)).rejects.toMatchObject({ code: 'invalid_response' });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    client.close();
  });
});

describe('incoming-route captured policy facts', () => {
  it('keeps incoming obligations separate from already covered channels, including exact capacity and omitted zeros', async () => {
    const client = await connected();
    for (const optional of [{}, { incoming_routes_pending: 0, incoming_routes_capacity: false }, { state: 'held', incoming_routes_pending: 1, reason: 'routed_findings_pending' }, { state: 'held', incoming_routes_pending: 100, reason: 'routed_findings_pending' }, { state: 'held', incoming_routes_capacity: true, reason: 'routed_findings_capacity' }, { state: 'unconfigured', policy_revision: 0, channels: [] }]) {
      const value = { ...assessment(), ...optional };
      const fetchMock = vi.fn().mockResolvedValueOnce(json(value)); vi.stubGlobal('fetch', fetchMock);
      const got = await client.reviewPolicyAssessment(evidenceID);
      expect(got).toEqual(value);
      if (got.state !== 'unconfigured') expect(got.channels).toEqual([{ channel: 'hosted', covered: true, pending: false }]);
      expect(fetchMock).toHaveBeenCalledTimes(1);
      const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
      expect(url).toBe(`https://service.example/api/v1/projects/project_one/reviews/evidence/${evidenceID}/policy`);
      expect(init.method).toBe('GET');
    }
    client.close();
  });

  const malformed: Array<[string, (v: Wire) => void]> = [
    ['false sufficient with incoming', v => { Object.assign(v, { incoming_routes_pending: 1, reason: 'routed_findings_pending' }); }],
    ['false sufficient with capacity', v => { Object.assign(v, { incoming_routes_capacity: true, reason: 'routed_findings_capacity' }); }],
    ['negative count', v => { v.incoming_routes_pending = -1; }],
    ['over 100 exact count', v => { Object.assign(v, { state: 'held', incoming_routes_pending: 101, reason: 'routed_findings_pending' }); }],
    ['fraction count', v => { v.incoming_routes_pending = 1.5; }],
    ['null count', v => { v.incoming_routes_pending = null; }],
    ['string count', v => { v.incoming_routes_pending = '1'; }],
    ['null capacity', v => { v.incoming_routes_capacity = null; }],
    ['string capacity', v => { v.incoming_routes_capacity = 'false'; }],
    ['null reason', v => { v.reason = null; }],
    ['pending without reason', v => { Object.assign(v, { state: 'held', incoming_routes_pending: 1 }); }],
    ['reason without pending', v => { Object.assign(v, { state: 'held', reason: 'routed_findings_pending' }); }],
    ['capacity with exact pending count', v => { Object.assign(v, { state: 'held', incoming_routes_pending: 1, incoming_routes_capacity: true, reason: 'routed_findings_capacity' }); }],
    ['capacity without reason', v => { Object.assign(v, { state: 'held', incoming_routes_capacity: true }); }],
    ['capacity reason without flag', v => { Object.assign(v, { state: 'held', reason: 'routed_findings_capacity' }); }],
    ['capacity with pending reason', v => { Object.assign(v, { state: 'held', incoming_routes_capacity: true, reason: 'routed_findings_pending' }); }],
    ['unconfigured with incoming', v => { Object.assign(v, { state: 'unconfigured', policy_revision: 0, channels: [], incoming_routes_pending: 1, reason: 'routed_findings_pending' }); }],
    ['unconfigured with silent incoming', v => { Object.assign(v, { state: 'unconfigured', policy_revision: 0, channels: [], incoming_routes_pending: 1 }); }],
    ['unconfigured with silent capacity', v => { Object.assign(v, { state: 'unconfigured', policy_revision: 0, channels: [], incoming_routes_capacity: true }); }],
    ['stale observed source is not a captured assessment', v => { Object.assign(v, { state: 'held', channels: [], reason: 'source_configuration_changed' }); }],
    ['stale reason cannot hide negative incoming', v => { Object.assign(v, { state: 'held', channels: [], reason: 'source_configuration_changed', incoming_routes_pending: -1 }); }],
  ];
  it.each(malformed)('rejects %s', async (_name, edit) => {
    const client = await connected();
    const value = assessment(); edit(value);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(value)));
    await expect(client.reviewPolicyAssessment(evidenceID)).rejects.toMatchObject({ code: 'invalid_response' });
    client.close();
  });

  it('rejects duplicate and case-aliased optional fields under the strict assessment decoder', async () => {
    const client = await connected();
    const base = JSON.stringify({ ...assessment(), state: 'held', incoming_routes_pending: 1, reason: 'routed_findings_pending' });
    for (const raw of [base.replace('"incoming_routes_pending":1', '"incoming_routes_pending":1,"incoming_routes_pending":1'), base.replace('"incoming_routes_pending":1', '"Incoming_routes_pending":1')]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(raw, { headers: { 'content-type': 'application/json' } })));
      await expect(client.reviewPolicyAssessment(evidenceID)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    client.close();
  });
});
