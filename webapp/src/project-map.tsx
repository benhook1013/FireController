import { useState } from 'react';
import { Link, useParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { Pager, ReadState, signalFor, usePaging, useWorkspace } from './App';
import { projectMapURL } from './api';
import type { JobWorkstream, ProjectMapEntry, ProjectMapKind, ProjectMapSummary, ProjectMapHistorySummary, ProjectMapRevision } from './types';

const pageSize = 20;
const entryPath = (home: string, id: string) => `${home}/map/entries/${encodeURIComponent(id)}`;
const date = (value: string) => new Date(value).toLocaleString();

export function ProjectMap() {
  const [kind, setKind] = useState<ProjectMapKind | ''>('');
  const [archived, setArchived] = useState(false);
  return <><div className="page-heading"><p className="eyebrow">Project direction</p><h2>Map</h2><p>Workstreams and return points. States are editorial notes, not completion or readiness verdicts.</p></div>
    <div className="filters"><label>Kind<select aria-label="Map entry kind" value={kind} onChange={event => setKind(event.target.value as ProjectMapKind | '')}><option value="">All kinds</option><option value="workstream">Workstreams</option><option value="return_point">Return points</option></select></label>
      <label className="checkbox-label"><input type="checkbox" checked={archived} onChange={event => setArchived(event.target.checked)} />Archived entries</label></div>
    <MapList key={`${kind}/${archived}`} kind={kind} archived={archived} />
  </>;
}

export function MapSummaryRows({ rows, home }: { rows: ProjectMapSummary[]; home: string }) {
  return <ul className="job-list">{rows.map(entry => <li key={entry.id}><div><span className="badge neutral">{entry.kind === 'workstream' ? 'Workstream' : 'Return point'}{entry.archived ? ' · Archived' : ''}</span><Link className="job-title" to={entryPath(home, entry.id)}>{entry.title}</Link><p className="full-text">{entry.state || 'No state supplied.'}</p><p className="item-meta">Revision {entry.revision}</p></div><Link className="detail-link" to={entryPath(home, entry.id)}>Read entry <span aria-hidden="true">→</span></Link></li>)}</ul>;
}

function MapList({ kind, archived }: { kind: ProjectMapKind | ''; archived: boolean }) {
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const query = useQuery({ queryKey: [...key, 'map', kind, archived, paging.cursor], queryFn: ({ signal }) => session.api.projectMapEntries({ limit: pageSize, archived, ...(kind ? { kind } : {}), ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  return <section className="panel"><ReadState error={query.error} loading={query.isPending} empty={!query.data?.items.length}><MapSummaryRows rows={query.data?.items ?? []} home={home} /></ReadState><Pager paging={paging} page={query.data} label="map entries" busy={query.isFetching} /></section>;
}

export function MapEntryContent({ entry }: { entry: ProjectMapEntry }) {
  return <section className="panel brief"><span className="badge neutral">{entry.kind === 'workstream' ? 'Workstream' : 'Return point'}{entry.archived ? ' · Archived' : ''}</span><h2>{entry.title}</h2><p className="item-meta">Revision {entry.revision}</p>
    <dl><dt>State</dt><dd className="full-text">{entry.state || 'No state supplied.'}</dd><dt>Description</dt><dd className="full-text">{entry.description || 'No description supplied.'}</dd>
      {entry.kind === 'workstream' ? <><dt>Where it stands</dt><dd className="full-text">{entry.where || 'Not supplied.'}</dd><dt>Next milestone</dt><dd className="full-text">{entry.next || 'Not supplied.'}</dd></> : <><dt>Reconsider when</dt><dd className="full-text">{entry.trigger || 'No trigger supplied.'}</dd></>}
    </dl>
    {entry.kind === 'workstream' && <><h3>Phases</h3>{entry.phases.length ? <ol>{entry.phases.map(phase => <li key={phase.id}><strong>{phase.name}</strong>{phase.archived && <span className="badge neutral">Archived</span>}<p className="full-text">{phase.state || 'No state supplied.'}</p></li>)}</ol> : <p className="empty">No phases recorded.</p>}</>}
    <h3>Links</h3>{entry.links.length ? <ul>{entry.links.map((link, index) => <li key={`${index}-${link.url}`}>{projectMapURL(link.url) ? <a href={link.url} target="_blank" rel="noreferrer noopener">{link.label}</a> : <span>{link.label} (unavailable link)</span>}</li>)}</ul> : <p className="empty">No links recorded.</p>}
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
