import { useState } from 'react';
import { Link, useParams } from 'react-router';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { Pager, ReadState, signalFor, usePaging, useWorkspace } from './workspace';
import { useProjectHeading } from './presentation-context';
import { projectMapURL } from './api';
import { EditorialMarkdown } from './editorial-markdown';
import type { JobWorkstream, ProjectMapEntry, ProjectMapKind, ProjectMapSummary, ProjectMapHistorySummary, ProjectMapRevision } from './types';

const pageSize = 20;
const entryPath = (home: string, id: string) => `${home}/map/entries/${encodeURIComponent(id)}`;
const date = (value: string) => new Date(value).toLocaleString();

export function ProjectMap() {
  const label = useProjectHeading('map');
  const [kind, setKind] = useState<ProjectMapKind | ''>('');
  const [archived, setArchived] = useState(false);
  const [catalogueOpen, setCatalogueOpen] = useState(false);
  return <section className="project-editorial">
    <header className="project-editorial-heading"><h2>{label}</h2></header>
    <div className="panel project-map-body">
    <ProjectEditorial />
    <details className="editorial-catalogue" onToggle={event => setCatalogueOpen(event.currentTarget.open)}>
      <summary>Browse the entry catalogue and archive</summary>
      <div className="editorial-catalogue-content">
        <p className="muted">The Project page above follows editorial order. This catalogue is for locating individual entries and archived history.</p>
        <div className="filters"><label>Kind<select aria-label="Map entry kind" value={kind} onChange={event => setKind(event.target.value as ProjectMapKind | '')}><option value="">All kinds</option><option value="section">Sections</option><option value="workstream">Workstreams</option><option value="return_point">Return points</option></select></label>
          <label className="checkbox-label"><input type="checkbox" checked={archived} onChange={event => setArchived(event.target.checked)} />Archived entries</label></div>
        {catalogueOpen && <MapList key={`${kind}/${archived}`} kind={kind} archived={archived} />}
      </div>
    </details>
    </div>
  </section>;
}

function ProjectEditorial() {
  const { session, key } = useWorkspace();
  const query = useInfiniteQuery({
    queryKey: [...key, 'map-editorial'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) => session.api.projectMapEditorial({ limit: 5, ...(pageParam ? { cursor: pageParam } : {}) }, signalFor(session, signal)),
    getNextPageParam: page => page.has_more ? page.cursor : undefined,
    staleTime: Infinity,
  });
  const entries = query.data?.pages.flatMap(page => page.items) ?? [];
  return <>
    {entries.length > 1 && <nav className="project-editorial-nav" aria-label="Project sections">{entries.map(entry => <a key={entry.id} href={`#${entry.id}`}>{entry.title}</a>)}</nav>}
    <ReadState error={entries.length ? undefined : query.error} loading={query.isPending} empty={false}>
      {entries.length ? <div className="project-editorial-body">{entries.map(entry => <ProjectEditorialEntry key={entry.id} entry={entry} />)}</div> : <p className="editorial-empty">No Project content has been added yet.</p>}
    </ReadState>
    {query.hasNextPage && <div className="editorial-more"><button type="button" disabled={query.isFetchingNextPage} onClick={() => { void query.fetchNextPage(); }}>{query.isFetchingNextPage ? 'Loading more…' : 'Load more Project content'}</button></div>}
    {query.isFetchNextPageError && <p className="notice" role="alert">More Project content could not be loaded. Try again.</p>}
  </>;
}

function kindLabel(kind: ProjectMapKind) {
  if (kind === 'section') return 'Section';
  if (kind === 'workstream') return 'Workstream';
  return 'Return point';
}

export function ProjectEditorialEntry({ entry }: { entry: ProjectMapEntry }) {
  return <article className={`project-editorial-entry ${entry.kind}`} id={entry.id}>
    <div className="editorial-entry-main">
      <header className="editorial-entry-heading">{entry.kind !== 'section' && <span className="editorial-entry-kind">{kindLabel(entry.kind)}</span>}<h3>{entry.title}</h3>{entry.state && <span className="editorial-entry-state">{entry.state}</span>}</header>
      {entry.description && <EditorialMarkdown>{entry.description}</EditorialMarkdown>}
      {entry.kind === 'workstream' && <>
        {(entry.where || entry.next) && <div className="editorial-entry-detail">
          {entry.where && <section><h4>Where it stands</h4><EditorialMarkdown>{entry.where}</EditorialMarkdown></section>}
          {entry.next && <section><h4>Next</h4><EditorialMarkdown>{entry.next}</EditorialMarkdown></section>}
        </div>}
        {entry.phases.length > 0 && <section className="editorial-phases"><h4>Phases</h4><ol className="editorial-phase-list">{entry.phases.map(phase => <li key={phase.id}><div className="editorial-phase-heading"><strong>{phase.name}</strong>{phase.archived && <span className="badge neutral">Archived</span>}{phase.state && <span className="editorial-entry-state">{phase.state}</span>}</div></li>)}</ol></section>}
      </>}
      {entry.kind === 'return_point' && entry.trigger && <section className="editorial-entry-detail"><div><h4>Reconsider when</h4><EditorialMarkdown>{entry.trigger}</EditorialMarkdown></div></section>}
      {entry.links.length > 0 && <section className="editorial-links"><h4>References</h4><ul className="editorial-link-list">{entry.links.map((link, index) => <li key={`${index}-${link.url}`}>{projectMapURL(link.url) ? <a href={link.url} target="_blank" rel="noreferrer noopener">{link.label}</a> : <span>{link.label} (unavailable link)</span>}</li>)}</ul></section>}
    </div>
  </article>;
}

export function MapSummaryRows({ rows, home }: { rows: ProjectMapSummary[]; home: string }) {
  return <ul className="job-list">{rows.map(entry => <li key={entry.id}><div><span className="badge neutral">{kindLabel(entry.kind)}{entry.archived ? ' · Archived' : ''}</span><Link className="job-title" to={entryPath(home, entry.id)}>{entry.title}</Link>{entry.state && <p className="full-text">{entry.state}</p>}<p className="item-meta">Revision {entry.revision}</p></div><Link className="detail-link" to={entryPath(home, entry.id)}>Read entry <span aria-hidden="true">→</span></Link></li>)}</ul>;
}

function MapList({ kind, archived }: { kind: ProjectMapKind | ''; archived: boolean }) {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'map', kind, archived, paging.cursor], queryFn: ({ signal }) => session.api.projectMapEntries({ limit: pageSize, archived, ...(kind ? { kind } : {}), ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><MapSummaryRows rows={query.data?.items ?? []} home={home} /></ReadState><Pager paging={paging} page={query.data} label="map entries" busy={query.isFetching} /></section>;
}

export function MapEntryContent({ entry }: { entry: ProjectMapEntry }) {
  return <section className="panel brief"><span className="badge neutral">{kindLabel(entry.kind)}{entry.archived ? ' · Archived' : ''}</span><h2 className="map-entry-title">{entry.title}</h2><p className="item-meta">Revision {entry.revision}</p>
    {entry.state && <p className="editorial-entry-state">{entry.state}</p>}
    {entry.description && <EditorialMarkdown>{entry.description}</EditorialMarkdown>}
    {entry.kind === 'workstream' && <>
      {entry.where && <section className="editorial-detail"><h3>Where it stands</h3><EditorialMarkdown>{entry.where}</EditorialMarkdown></section>}
      {entry.next && <section className="editorial-detail"><h3>Next</h3><EditorialMarkdown>{entry.next}</EditorialMarkdown></section>}
      {entry.phases.length > 0 && <section className="editorial-phases"><h3>Phases</h3><ol className="editorial-phase-list">{entry.phases.map(phase => <li key={phase.id}><div className="editorial-phase-heading"><strong>{phase.name}</strong>{phase.archived && <span className="badge neutral">Archived</span>}</div>{phase.state && <EditorialMarkdown>{phase.state}</EditorialMarkdown>}</li>)}</ol></section>}
    </>}
    {entry.kind === 'return_point' && entry.trigger && <section className="editorial-detail"><h3>Reconsider when</h3><EditorialMarkdown>{entry.trigger}</EditorialMarkdown></section>}
    {entry.links.length > 0 && <section className="editorial-links"><h3>References</h3><ul className="editorial-link-list">{entry.links.map((link, index) => <li key={`${index}-${link.url}`}>{projectMapURL(link.url) ? <a href={link.url} target="_blank" rel="noreferrer noopener">{link.label}</a> : <span>{link.label} (unavailable link)</span>}</li>)}</ul></section>}
    <dl className="metadata"><div><dt>Entry ID</dt><dd>{entry.id}</dd></div><div><dt>Created</dt><dd><time dateTime={entry.created_at}>{date(entry.created_at)}</time></dd></div><div><dt>Updated</dt><dd><time dateTime={entry.updated_at}>{date(entry.updated_at)}</time></dd></div></dl>
  </section>;
}

export function ProjectMapEntryView() {
  const { entryId = '' } = useParams();
  const { session, key, home } = useWorkspace();
  const [history, setHistory] = useState(false);
  const query = useQuery({ queryKey: [...key, 'map-entry', entryId], queryFn: ({ signal }) => session.api.projectMapEntry(entryId, signalFor(session, signal)), staleTime: Infinity });
  return <><Link className="back-link" to={`${home}/map`}>← All map entries</Link><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <><MapEntryContent entry={query.data} /><section className="panel"><button type="button" aria-expanded={history} onClick={() => setHistory(value => !value)}>{history ? 'Close map history' : 'Show map history'}</button>{history && <MapHistory key={entryId} entryId={entryId} />}</section></>}</ReadState></>;
}

function MapHistory({ entryId }: { entryId: string }) {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'map-history', entryId, paging.cursor], queryFn: ({ signal }) => session.api.projectMapHistory(entryId, { limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  return <div className="brief"><h3>Map revision history</h3><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><MapHistoryRows rows={query.data?.items ?? []} home={home} entryId={entryId} /></ReadState><Pager paging={paging} page={query.data} label="map history" busy={query.isFetching} /></div>;
}

export function MapHistoryRows({ rows, home, entryId }: { rows: ProjectMapHistorySummary[]; home: string; entryId: string }) {
  return <ul>{rows.map(row => <li key={row.revision}><Link to={`${entryPath(home, entryId)}/history/${row.revision}`}>Read revision {row.revision}</Link><p className="item-meta">Recorded by {row.actor_id} · <time dateTime={row.created_at}>{date(row.created_at)}</time></p></li>)}</ul>;
}

export function MapRevisionContent({ value }: { value: ProjectMapRevision }) {
  return <><div className="notice" role="status">Captured revision {value.revision}. This historical snapshot does not describe current state.</div><p className="item-meta">Recorded by {value.actor_id} · <time dateTime={value.created_at}>{date(value.created_at)}</time></p><MapEntryContent entry={value.state} /></>;
}

export function ProjectMapRevisionView() {
  const { entryId = '', revision = '' } = useParams();
  const { session, key, home } = useWorkspace();
  const selected = /^[1-9]\d*$/u.test(revision) ? Number(revision) : NaN;
  const query = useQuery({ queryKey: [...key, 'map-revision', entryId, revision], queryFn: ({ signal }) => session.api.projectMapRevision(entryId, selected, signalFor(session, signal)), staleTime: Infinity });
  return <><Link className="back-link" to={entryPath(home, entryId)}>← Current map entry</Link><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <MapRevisionContent value={query.data} />}</ReadState></>;
}

export function WorkstreamReferenceContent({ value, home }: { value: JobWorkstream; home: string }) {
  return value.workstream_id ? <><p className="full-text">Recorded title: {value.workstream_title}</p><p className="muted">The current workstream title or archive state may have changed.</p><Link to={entryPath(home, value.workstream_id)}>Open current workstream</Link><p className="item-meta">Association revision {value.revision}</p></> : <p className="empty">No workstream reference is recorded for this job.</p>;
}

export function JobWorkstreamPanel({ jobId }: { jobId: string }) {
  const [open, setOpen] = useState(false);
  return <section className="panel"><div className="section-heading"><h3>Workstream reference</h3><button type="button" aria-expanded={open} onClick={() => setOpen(value => !value)}>{open ? 'Close workstream reference' : 'Show workstream reference'}</button></div>{open && <JobWorkstreamRead jobId={jobId} />}</section>;
}

function JobWorkstreamRead({ jobId }: { jobId: string }) {
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'job-workstream', jobId], queryFn: ({ signal }) => session.api.jobWorkstream(jobId, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  return <ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <WorkstreamReferenceContent value={query.data} home={home} />}</ReadState>;
}
