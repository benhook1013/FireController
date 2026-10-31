import { useEffect, useLayoutEffect, useMemo, useState } from 'react';
import { Link, Route, Routes, useParams, useSearchParams } from 'react-router';
import { PublicSiteApi, publicProjectLocation, validPublicAddress, validPublicGeneration, validPublicSlug, type PublicCard, type PublicHistory, type PublicLink, type PublicSnapshot } from './public-site-api';
import { presentationDefaults, type ThemeToken } from './presentation';
import flameEmber from './flame-ember.svg?no-inline';
import './public-site.css';
import { EditorialMarkdown } from './editorial-markdown';
import { PublicReviews } from './public-reviews';
import { PublicWorkerLanes } from './public-worker-lanes';
import { PublicHistorySkeleton, PublicProjectSkeleton } from './loading';

export function PublicSiteRouter() {
  return <Routes><Route path="/public/:slug" element={<PublicProjectSite />} /><Route path="/public/:slug/history/:revision" element={<PublicProjectSite />} /><Route path="*" element={<PublicUnavailable />} /></Routes>;
}
export function PublicUnavailable() { return <main className="public-unavailable"><img className="flame" src={flameEmber} alt="" /><h1>Project unavailable</h1><p>This project has not been published, or its shared link is no longer available.</p><Link to="/">Sign in to the workspace</Link></main>; }
function date(value: string) { return new Date(value).toLocaleString(); }
function PublicLinks({ links }: { links: PublicLink[] }) { if (!links.length) return null; return <ul className="public-links">{links.map(link => <li key={link.key}><a href={link.url} target="_blank" rel="noreferrer noopener">{link.label}<span className="public-link-destination">{link.url}</span></a></li>)}</ul>; }
function Cards({ title, cards, anchor }: { title: string; cards: PublicCard[]; empty: string; anchor: string }) {
  if (!cards.length) return null;
  const narrative = anchor === 'project-map';
  return <section id={anchor} className="public-section"><div className="section-heading"><h2>{title}</h2></div><div className={narrative ? 'public-editorial' : 'public-card-grid'}>{cards.map(card => <article className={narrative ? 'public-story' : 'panel public-card'} key={card.key}>{(card.phase || card.status) && <div className="item-meta">{card.phase && <span>{card.phase}</span>}{card.status && <span className="public-status">{card.status}</span>}</div>}<h3>{card.title}</h3>{card.description && <EditorialMarkdown>{card.description}</EditorialMarkdown>}<PublicLinks links={card.links} /></article>)}</div></section>;
}
export function PublicSnapshotView({ snapshot, historical = false, status = 'live', refreshing = false, refresh, history, browseHistory, embedded = false, home = `/projects/${snapshot.slug}`, signIn }: { snapshot: PublicSnapshot; embedded?: boolean; home?: string; signIn?: () => void; historical?: boolean; status?: string; refreshing?: boolean; refresh?: () => void; history?: React.ReactNode; browseHistory?: () => void }) {
  const { content } = snapshot, presentation = content.presentation;
  const Layout = embedded ? 'div' : 'main';
  return <div className="public-project">{!embedded && <header className="mast"><div className="mast-inner"><Link className="brand" to={home}><img className="flame" src={flameEmber} alt="" aria-hidden="true" />{presentation.brand_name}</Link><span className="mast-label">{presentation.tagline}</span><span className="public-signin">{signIn ? <button type="button" onClick={signIn}>Sign in</button> : <Link to={`${home}?signin=1`}>Sign in</Link>}</span></div></header>}
    <Layout className="public-layout"><section className="public-hero"><div><p className="eyebrow">{historical ? 'Published history' : presentation.headings.hero_eyebrow ?? 'Project overview'}</p><h1>{presentation.headings.hero_title ?? presentation.name}</h1><p>{presentation.headings.hero_description ?? presentation.tagline}</p><PublicLinks links={content.links} /></div><div className="public-hero-status"><span className="public-hero-pill">{historical ? `Revision ${snapshot.revision} · frozen` : status === 'live' ? 'Live published view' : status === 'reconnecting' ? 'Reconnecting' : status === 'closed' ? 'Live connection ended' : 'Connecting'}</span><span>Published <time dateTime={snapshot.published_at}>{date(snapshot.published_at)}</time></span>{refresh && <button disabled={refreshing} onClick={refresh}>{refreshing ? 'Refreshing…' : 'Refresh published data'}</button>}</div></section>
    {historical && <p className="notice">This approved revision is frozen. <Link to={home}>View the current publication</Link>.</p>}
    <nav className="public-navigation" aria-label="Public project sections">{content.project_map.length > 0 && <a href="#project-map">{presentation.headings.map ?? 'Project'}</a>}{content.progress.length > 0 && <a href="#progress">Progress</a>}<a href="#worker-lanes">{presentation.headings.workers ?? 'Worker lanes'}</a><a href="#reviews">{presentation.headings.reviews ?? 'Review queue'}</a><a href="#published-history">Published history</a></nav>
    {content.description && <section className="public-section"><h2>About {presentation.name}</h2><EditorialMarkdown>{content.description}</EditorialMarkdown></section>}
    <Cards title={presentation.headings.map ?? 'Project'} cards={content.project_map} anchor="project-map" empty="No project map entries have been published." /><Cards title="Progress" cards={content.progress} anchor="progress" empty="No progress summaries have been published." />
    <section id="worker-lanes" className="public-section"><h2>{presentation.headings.workers ?? 'Worker lanes'}</h2><PublicWorkerLanes lanes={content.worker_lanes} /></section>
    <section id="reviews" className="public-section"><h2>{presentation.headings.reviews ?? 'Review queue'}</h2><p className="muted">Finalized historical summaries. Counts are found / accepted / routed; these summaries do not establish current review coverage or merge readiness.</p><PublicReviews reviews={content.reviews} browseHistory={browseHistory} /></section>
    <section id="published-history" className="public-section"><h2>Published history</h2>{history ?? <p className="muted">Only explicitly published revisions are available.</p>}</section></Layout>{!embedded && <footer className="footer"><span>Powered by <a href="https://github.com/benhook1013/FireController" target="_blank" rel="noreferrer noopener">FireController</a></span><span>Public project website</span></footer>}</div>;
}
function PublicProjectSite() {
  const { slug = '', revision: rawRevision } = useParams(), [query] = useSearchParams();
  const generation = query.get('generation') ?? undefined, revision = rawRevision === undefined ? undefined : Number(rawRevision);
  const valid = validPublicSlug(slug) && (rawRevision === undefined ? query.toString() === '' : Number.isSafeInteger(revision) && Number(revision) > 0 && String(revision) === rawRevision && validPublicGeneration(generation) && [...query.keys()].length === 1);
  if (!valid) return <PublicUnavailable />;
  return <PublicProjectBinding key={`${slug}:${rawRevision ?? ''}:${generation ?? ''}`} slug={slug} revision={revision} generation={generation} />;
}
export function PublicProjectAddress({ address, ...props }: Omit<PublicProjectProps, 'slug'> & { address: string }) {
  const [slug, setSlug] = useState<string>(), [failed, setFailed] = useState(false), [retry, setRetry] = useState(0), [notice, setNotice] = useState('');
  useEffect(() => {
    const controller = new AbortController(); setSlug(undefined); setFailed(false); setNotice('');
    void publicProjectLocation(address, controller.signal).then(value => { if (!controller.signal.aborted) setSlug(value); }).catch(error => {
      if (controller.signal.aborted) return;
      if (error && typeof error === 'object' && 'status' in error && error.status === 404) setFailed(true);
      else setNotice('The published view could not be loaded. Try again.');
    });
    return () => controller.abort();
  }, [address, retry]);
  if (!validPublicAddress(address) || failed) return props.fallback ?? <PublicUnavailable />;
  if (!slug) return <PublicProjectLoadState embedded={props.embedded} notice={notice} retry={() => { setNotice(''); setRetry(value => value + 1); }} />;
  return <PublicProjectBinding key={`${slug}:${props.revision ?? ''}:${props.generation ?? ''}`} {...props} slug={slug} home={`/projects/${encodeURIComponent(address)}`} />;
}
type PublicProjectProps = { slug: string; revision?: number; generation?: string; embedded?: boolean; fallback?: React.ReactNode; theme?: boolean; home?: string };
export function PublicProjectLoadState({ embedded = false, notice = '', retry, signIn }: { embedded?: boolean; notice?: string; retry: () => void; signIn?: string }) {
  if (!notice) return <PublicProjectSkeleton embedded={embedded} />;
  const Layout = embedded ? 'div' : 'main';
  return <Layout className="public-unavailable"><p role="alert">{notice}</p><button type="button" onClick={retry}>Try again</button>{signIn && <Link to={signIn}>Sign in</Link>}</Layout>;
}
export function PublicProjectBinding(props: PublicProjectProps) {
  return validPublicSlug(props.slug) ? <PublicProjectContent {...props} /> : props.fallback ?? <PublicUnavailable />;
}
function PublicProjectContent({ slug, revision, generation, embedded = false, fallback, theme = true, home }: PublicProjectProps) {
  const api = useMemo(() => new PublicSiteApi(slug), [slug]);
  const [snapshot, setSnapshot] = useState<PublicSnapshot | null>(null), [status, setStatus] = useState('connecting'), [failed, setFailed] = useState(false), [notice, setNotice] = useState(''), [refreshing, setRefreshing] = useState(false), [refresh, setRefresh] = useState(0), [historyRequested, setHistoryRequested] = useState('');
  useEffect(() => {
    const controller = new AbortController(); let active = true;
    setRefreshing(true); setNotice('');
    const fail = (error: unknown) => { if (!active) return; setStatus('closed'); setRefreshing(false); if (error && typeof error === 'object' && 'status' in error && (error.status === 404 || error.status === 409 || error.status === 403)) { setSnapshot(null); setFailed(true); } else setNotice('The published view could not be updated. Try refreshing.'); };
    if (revision !== undefined) void api.snapshot(controller.signal, revision, generation).then(value => { if (active) { setSnapshot(value); setRefreshing(false); } }).catch(fail);
    else void api.watch({ snapshot: value => { if (active) { setSnapshot(value); setRefreshing(false); } }, status: value => { if (active) setStatus(value); } }, controller.signal).catch(fail);
    return () => { active = false; controller.abort(); };
  }, [api, revision, generation, refresh]);
  useLayoutEffect(() => {
    if (!snapshot || !theme) return;
    const oldTitle = document.title, root = document.documentElement, previous = new Map<string, string>();
    for (const key of Object.keys(presentationDefaults.theme) as ThemeToken[]) { const name = `--fc-${key}`; previous.set(name, root.style.getPropertyValue(name)); root.style.setProperty(name, snapshot.content.presentation.theme[key] ?? presentationDefaults.theme[key]); }
    document.title = `${snapshot.content.presentation.name} · FireController`;
    return () => { document.title = oldTitle; for (const [key, value] of previous) { if (value) root.style.setProperty(key, value); else root.style.removeProperty(key); } };
  }, [snapshot, theme]);
  if (failed) return fallback ?? <PublicUnavailable />;
  if (!snapshot) return <PublicProjectLoadState embedded={embedded} notice={notice} retry={() => { setNotice(''); setRefresh(value => value + 1); }} signIn={embedded ? undefined : `/projects/${slug}?signin=1`} />;
  return <><PublicSnapshotView snapshot={snapshot} home={home} embedded={embedded} historical={revision !== undefined} status={status} refreshing={refreshing} refresh={() => setRefresh(n => n + 1)} browseHistory={() => setHistoryRequested(snapshot.generation)} history={<PublishedHistory key={snapshot.generation} home={home ?? `/projects/${snapshot.slug}`} api={api} snapshot={snapshot} requested={historyRequested === snapshot.generation} onRequest={() => setHistoryRequested(snapshot.generation)} />} />{notice && <div className="public-notice" role="alert">{notice}</div>}</>;
}
function PublishedHistory({ api, snapshot, requested, onRequest, home }: { home: string; api: PublicSiteApi; snapshot: PublicSnapshot; requested: boolean; onRequest: () => void }) {
  const [page, setPage] = useState<PublicHistory | null>(null), [error, setError] = useState(false), [cursor, setCursor] = useState<string>();
  useEffect(() => { if (!requested) return; const controller = new AbortController(); setError(false); void api.history(controller.signal, snapshot.generation, cursor).then(value => { if (!controller.signal.aborted) setPage(value); }).catch(() => { if (!controller.signal.aborted) setError(true); }); return () => controller.abort(); }, [api, snapshot, requested, cursor]);
  return <>{!requested ? <button onClick={onRequest}>Browse published revisions</button> : error ? <p role="alert">Published history is unavailable. Return to the current project view to reconnect.</p> : !page ? <PublicHistorySkeleton /> : <><ul className="public-history">{page.items.map(item => <li key={item.revision}><Link to={`${home}/published/history/${item.revision}?generation=${snapshot.generation}`}>Revision {item.revision}</Link> <time dateTime={item.published_at}>{date(item.published_at)}</time></li>)}</ul><div className="public-history-controls">{cursor && <button onClick={() => setCursor(undefined)}>Newest published revisions</button>}{page.has_more && <button onClick={() => setCursor(page.cursor)}>Older published revisions</button>}</div></>}</>;
}
