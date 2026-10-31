import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { EditorialMarkdown } from './editorial-markdown';
import { PageSkeleton } from './loading';
import { errorText, signalFor, usePaging, useWorkspace } from './workspace';

export type AdoptionObligationKind = 'finding' | 'route' | 'result';
export type AdoptionObligationItem = {
  id: string;
  namespace: string;
  source_key: string;
  kind: AdoptionObligationKind;
  status: 'open' | 'resolved';
  revision: number;
  affected_pull_numbers: number[];
};
export type AdoptionObligationPage = { items: AdoptionObligationItem[]; next_cursor?: string };
export type AdoptionObligationDetail = {
  id: string;
  project_id: string;
  source: { namespace: string; repository_id: number; snapshot_sha256: string };
  source_revision: number;
  original: {
    source_key: string;
    kind: AdoptionObligationKind;
    affected_pull_numbers: number[];
    summary: string;
    evidence: string[];
    original_status: 'pending' | 'open';
  };
  status: 'open' | 'resolved';
  revision: number;
  actor_id: string;
  worker_id: string;
  created_at: string;
  resolution: null | {
    revision: number;
    reason: string;
    evidence: string[];
    actor_id: string;
    worker_id: string;
    resolved_at: string;
  };
};

const kindLabel: Record<AdoptionObligationKind, string> = {
  finding: 'Finding', route: 'Routed finding', result: 'Unsettled result',
};

function Evidence({ items }: { items: string[] }) {
  return items.length
    ? <ul>{items.map((item, index) => <li key={index}><code className="full-text">{item}</code></li>)}</ul>
    : <p className="muted">No original evidence pointers were retained.</p>;
}

export function CarriedObligationContent({ value }: { value: AdoptionObligationDetail }) {
  return <div className="review-history-findings">
    <p className="item-meta">{value.status === 'resolved' ? 'Resolved after cutover' : 'Open carried work'} · originally {value.original.original_status}</p>
    <EditorialMarkdown className="full-text">{value.original.summary}</EditorialMarkdown>
    <h4>Original evidence</h4><Evidence items={value.original.evidence} />
    {value.resolution && <section aria-label="Carried work resolution"><h4>Resolution</h4>
      <EditorialMarkdown className="full-text">{value.resolution.reason}</EditorialMarkdown>
      <Evidence items={value.resolution.evidence} />
    </section>}
    <details><summary>Source details</summary><dl>
      <div><dt>Source</dt><dd>{value.source.namespace}</dd></div>
      <div><dt>Original key</dt><dd><code>{value.original.source_key}</code></dd></div>
      <div><dt>Affected PRs</dt><dd>{value.original.affected_pull_numbers.map(number => `#${number}`).join(', ')}</dd></div>
      <div><dt>Source snapshot</dt><dd><code className="full-text">{value.source.snapshot_sha256}</code></dd></div>
    </dl></details>
  </div>;
}

function CarriedObligationDetail({ id }: { id: string }) {
  const { session, key, fail } = useWorkspace();
  const query = useQuery({
    queryKey: [...key, 'reviews', 'current', 'adoption-obligation', id],
    queryFn: ({ signal }) => session.api.reviewObligation(id, signalFor(session, signal)),
    staleTime: Infinity, gcTime: 0, retry: false,
  });
  useEffect(() => { if (query.error) fail(query.error); }, [query.error, fail]);
  if (query.error) return <div role="alert" className="notice"><p>Carried review work could not be loaded. {errorText(query.error)}</p><button type="button" disabled={query.isFetching} onClick={() => { void query.refetch(); }}>Retry carried detail</button></div>;
  if (!query.data) return <PageSkeleton rows={2} label="Loading carried review work" />;
  return <CarriedObligationContent value={query.data} />;
}

function CarriedObligationRow({ item }: { item: AdoptionObligationItem }) {
  const [open, setOpen] = useState(false);
  return <li className="review-history-row"><details open={open} onToggle={event => setOpen(event.currentTarget.open)}>
    <summary aria-label={`Read carried ${kindLabel[item.kind].toLowerCase()} ${item.source_key}`}><strong>{kindLabel[item.kind]}</strong> · <code>{item.source_key}</code></summary>
    {open && <CarriedObligationDetail id={item.id} />}
  </details></li>;
}

function ReviewObligationList({ number }: { number: number }) {
  const { session, key, fail } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({
    queryKey: [...key, 'reviews', 'current', 'adoption-obligations', number, paging.cursor],
    queryFn: ({ signal }) => session.api.reviewObligations(number, { status: 'open', limit: 20, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)),
    staleTime: Infinity, retry: false,
  });
  useEffect(() => { if (query.error) fail(query.error); }, [query.error, fail]);
  const page = query.data;
  if (page && !page.items.length && paging.number === 1 && !page.next_cursor && !query.error) return null;
  return <section className="panel review-history" aria-label={`PR #${number} carried review work`}>
    <h3>Carried review work</h3>
    {query.error ? <div role="alert" className="notice"><p>Carried review work could not be checked. {errorText(query.error)}</p><button type="button" disabled={query.isFetching} onClick={() => { void query.refetch(); }}>Retry carried work</button></div>
      : !page ? <PageSkeleton rows={2} label="Loading carried review work" />
        : <>
          {page.items.length ? <>
            <p><strong>{page.items.length} open {page.items.length === 1 ? 'item' : 'items'}{paging.number > 1 || page.next_cursor ? ' shown' : ''}</strong> carried over at cutover. These hold new reviews until resolved. Check the original evidence against current code.</p>
            <ul className="review-history-run-list">{page.items.map(item => <CarriedObligationRow key={item.id} item={item} />)}</ul>
          </> : <p className="review-empty">{paging.number === 1 ? 'No open carried review work for this PR.' : 'No more open carried review work on this page.'}</p>}
          {(paging.number > 1 || page.next_cursor) && <div className="pager"><span>Page {paging.number}</span><div>
            <button type="button" aria-label="Previous carried obligations" disabled={paging.number === 1 || query.isFetching} onClick={paging.previous}>Previous</button>
            <button type="button" aria-label="Next carried obligations" disabled={!page.next_cursor || query.isFetching} onClick={() => { if (page.next_cursor) paging.next(page.next_cursor); }}>Next</button>
          </div></div>}
        </>}
  </section>;
}

export function ReviewObligations({ number }: { number: number }) {
  return <ReviewObligationList key={number} number={number} />;
}
