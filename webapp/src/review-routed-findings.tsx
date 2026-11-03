import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';
import { ApiError } from './api';
import { terminalBrowserSession } from './browser-session';
import { EditorialMarkdown } from './editorial-markdown';
import { CompactReviewFinding } from './review-finding-display';
import { CarriedObligationContent } from './review-obligations';
import { errorText, Pager, signalFor, usePaging, useWorkspace } from './workspace';
import type { ReviewFinding } from './types';
import './review-routed-findings.css';

export type ReviewRoutedFindingsOptions = { pullNumber?: number; cursor?: string; limit?: number };
export type ReviewRoutedFindingItem = {
  id: string;
  kind: 'native' | 'carried';
  state: 'open' | 'fixed' | 'rejected' | 'resolved';
  source_pull_numbers: number[];
  target_pull_number?: number;
  target_state: 'open' | 'closed' | 'merged' | 'unknown' | 'unassigned';
  title: string;
  revision: number;
  created_at: string;
  source_evidence_id?: string;
};
export type ReviewRoutedFindingsPage = {
  project_id: string;
  view: 'pull' | 'attention';
  pull_number: number;
  counts: { open: number; fixed: number; rejected: number; resolved: number };
  items: ReviewRoutedFindingItem[];
  cursor?: string;
  has_more: boolean;
};
export type ReviewRoutedFindingDetail = {
  route: {
    id: string; project_id: string; repository_id: number; source_evidence_id: string; finding: number;
    source_pull_number: number; source_head_sha: string; target_pull_number: number;
    state: 'active' | 'superseded'; revision: number; disposition: 'unadjudicated' | 'accepted' | 'rejected';
    routing_reason: string; routing_actor_id: string; reason: string; actor_id: string; decided_at: string;
    fix_sha: string; proof_note: string; fixed_at: string; created_at: string; updated_at: string;
  };
  source_anchor: {
    repository_id: number; repository: string; pull_number: number; candidate_sha: string; base_sha: string;
    parent_sha: string; parent_pull_number: number; base_ref: string; merge_base_sha: string; patch_sha256: string; observed_at: string;
  };
  source_finding: { finding: number; title: string; detail: string; severity: ReviewFinding['severity']; path: string; start_line: number; end_line: number };
};

function ReadFailure({ error, retry, busy, detail = false }: { error: unknown; retry: () => void; busy: boolean; detail?: boolean }) {
  const terminal = error instanceof ApiError && terminalBrowserSession(error);
  return <div className="review-routed-error" role="alert"><p>{detail ? 'Routed finding details could not be loaded.' : 'Routed findings could not be checked.'} {errorText(error)}</p>{!terminal && <button type="button" disabled={busy} onClick={retry}>{detail ? 'Retry routed detail' : 'Retry routed findings'}</button>}</div>;
}

export function NativeRoutedFindingContent({ value }: { value: ReviewRoutedFindingDetail }) {
  const { route, source_finding: source } = value;
  const finding: ReviewFinding = {
    source_finding_id: route.id, title: source.title, detail: source.detail, severity: source.severity,
    disposition: route.disposition, decision_reason: route.reason, actor: route.actor_id, decided_at: route.decided_at,
    fix_sha: route.fix_sha, proof_note: route.proof_note, resolved_at: route.fixed_at,
  };
  return <div className="review-routed-detail">
    <section className="review-routed-reason"><h4>Routing reason</h4><EditorialMarkdown compact>{route.routing_reason}</EditorialMarkdown></section>
    <ul className="review-routed-finding-content"><CompactReviewFinding finding={finding} /></ul>
    <details className="review-routed-source"><summary>Original comparison</summary><dl>
      <div><dt>Repository</dt><dd>{value.source_anchor.repository}</dd></div>
      <div><dt>Source head</dt><dd><code>{value.source_anchor.candidate_sha}</code></dd></div>
      <div><dt>Source base</dt><dd><code>{value.source_anchor.base_sha}</code></dd></div>
      {source.path && <div><dt>Location</dt><dd>{source.path}{source.start_line > 0 && `:${source.start_line}${source.end_line > source.start_line ? `–${source.end_line}` : ''}`}</dd></div>}
    </dl></details>
  </div>;
}

export function ReviewRoutedFindingDetails({ item }: { item: ReviewRoutedFindingItem }) {
  const { session, key, fail } = useWorkspace();
  const query = useQuery({
    queryKey: [...key, 'reviews', 'current', 'routed-findings', 'detail', item.kind, item.id, item.revision],
    queryFn: async ({ signal }) => {
      if (item.kind === 'carried') {
        const value = await session.api.reviewObligation(item.id, signalFor(session, signal));
        if (value.id !== item.id || value.project_id !== session.project.id || value.original.kind !== 'route' || value.revision < item.revision) throw new ApiError('invalid_response', 'The routed obligation detail did not match the selected record.');
        return { kind: 'carried' as const, value };
      }
      const value = await session.api.reviewRoute(item.id, signalFor(session, signal));
      if (value.route.id !== item.id || value.route.project_id !== session.project.id || value.route.revision < item.revision || value.route.target_pull_number !== item.target_pull_number || !item.source_pull_numbers.includes(value.route.source_pull_number) || item.source_evidence_id && value.route.source_evidence_id !== item.source_evidence_id) throw new ApiError('invalid_response', 'The route detail did not match the selected record.');
      return { kind: 'native' as const, value };
    },
    staleTime: Infinity, gcTime: 5 * 60 * 1000, retry: false,
  });
  useEffect(() => { if (query.error) fail(query.error); }, [query.error, fail]);
  if (query.error) return <ReadFailure error={query.error} retry={() => { void query.refetch(); }} busy={query.isFetching} detail />;
  if (!query.data) return <p className="review-routed-checking" role="status">Loading finding details…</p>;
  return query.data.kind === 'native' ? <NativeRoutedFindingContent value={query.data.value} /> : <CarriedObligationContent value={query.data.value} />;
}

function RoutedFindingRow({ item, number }: { item: ReviewRoutedFindingItem; number?: number }) {
  const { home } = useWorkspace();
  const [open, setOpen] = useState(false);
  const direction = number === undefined ? '' : item.target_pull_number === number ? 'Incoming' : item.source_pull_numbers.includes(number) ? 'Outgoing' : 'Carried route';
  const states = { open: 'Open', fixed: 'Fixed', rejected: 'Rejected', resolved: 'Resolved' };
  const issue = item.target_state === 'closed' ? 'Receiving PR is closed; this route remains open.'
    : item.target_state === 'merged' ? 'Receiving PR is merged; this route remains open.'
      : item.target_state === 'unassigned' ? 'No receiving PR is assigned.'
        : item.target_state === 'unknown' ? 'Receiving PR status is unknown.' : '';
  return <li className={`review-routed-item review-routed-${item.state}`}>
    <div className="review-routed-item-heading"><h4>{item.title || (item.kind === 'carried' ? 'Carried routed finding' : 'Routed finding')}</h4><span className="review-routed-state">{states[item.state]}</span></div>
    <div className="review-routed-direction">{direction && <strong>{direction}</strong>}<span>Source: {item.source_pull_numbers.length ? item.source_pull_numbers.map((pull, index) => <span key={pull}>{index > 0 && ', '}<Link to={`${home}/reviews/pulls/${pull}`}>PR #{pull}</Link></span>) : 'PR unknown'}</span><span aria-hidden="true">→</span><span>Receiving: {item.target_pull_number ? <Link to={`${home}/reviews/pulls/${item.target_pull_number}`}>PR #{item.target_pull_number}</Link> : item.target_state === 'unassigned' ? 'Unassigned' : 'Unknown receiving PR'}</span></div>
    {item.state === 'open' && issue && <p className="review-routed-attention-reason">{issue}</p>}
    <details className="review-routed-disclosure" open={open} onToggle={event => setOpen(event.currentTarget.open)}><summary>Finding details and proof</summary>{open && <ReviewRoutedFindingDetails item={item} />}</details>
  </li>;
}

function RoutedFindingsList({ number }: { number?: number }) {
  const { session, key, fail } = useWorkspace();
  const paging = usePaging(JSON.stringify([key, number ?? 'attention']));
  const query = useQuery({
    queryKey: [...key, 'reviews', 'current', 'routed-findings', number ?? 'attention', paging.cursor],
    queryFn: ({ signal }) => session.api.reviewRoutedFindings({ ...(number === undefined ? {} : { pullNumber: number }), limit: 20, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)),
    staleTime: Infinity, retry: false,
  });
  useEffect(() => { if (query.error) fail(query.error); }, [query.error, fail]);
  const page = query.data;
  if (number === undefined && !query.error && (!page || paging.number === 1 && !page.items.length && !page.has_more && Object.values(page.counts).every(count => count === 0))) return null;
  return <section className="panel review-routed-findings" aria-label={number === undefined ? 'Routed findings requiring attention' : `PR #${number} routed findings`}>
    <div className="review-routed-heading"><h3>Routed Findings</h3>{!query.error && page && <div className="review-routed-counts" aria-label="All matching routed finding counts"><span className="review-routed-count review-routed-count-open">Open: <strong>{page.counts.open}</strong></span><span className="review-routed-count review-routed-count-fixed">Fixed: <strong>{page.counts.fixed}</strong></span><span className="review-routed-count review-routed-count-rejected">Rejected: <strong>{page.counts.rejected}</strong></span>{page.counts.resolved > 0 && <span className="review-routed-count review-routed-count-resolved">Resolved: <strong>{page.counts.resolved}</strong></span>}</div>}</div>
    {query.error ? <ReadFailure error={query.error} retry={() => { void query.refetch(); }} busy={query.isFetching} /> : !page ? <p className="review-routed-checking" role="status">Checking routed findings…</p> : <>
      {page.items.length ? <ul className="review-routed-list">{page.items.map(item => <RoutedFindingRow key={`${item.kind}:${item.id}`} item={item} number={number} />)}</ul> : <p className="review-routed-empty">{paging.number > 1 ? 'No more routes on this page.' : 'No routes recorded.'}</p>}
      {(paging.number > 1 || page.has_more) && <Pager paging={paging} page={page} label="routed findings" busy={query.isFetching} />}
    </>}
  </section>;
}

export function ReviewRoutedFindings({ number }: { number?: number }) {
  return <RoutedFindingsList key={number ?? 'attention'} number={number} />;
}
