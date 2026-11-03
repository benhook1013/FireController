import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import App from './App';
import { PublicSnapshotView } from './public-site';
import type { PublicSnapshot } from './public-site-api';

function fixture(): PublicSnapshot { const generation = `public_generation_${'a'.repeat(32)}`; return { slug: 'showcase', generation, revision: 2, published_at: '2026-10-17T12:00:00Z', cursor: `${generation}:2`, content: { slug: 'showcase', presentation: { name: 'Showcase', brand_name: 'Showcase', tagline: 'A project worth sharing', headings: { map: 'Roadmap' }, theme: {} }, description: '<script>literal approved text</script>', links: [{ key: 'source', label: 'Project source', url: 'https://example.com/project' }], project_map: [{ key: 'build', title: 'Build the project', description: 'Approved roadmap description', phase: 'Build', status: 'Active', links: [] }], progress: [], worker_lanes: [{ key: 'builder', label: 'Builder lane', headline: 'Shipping the project', status: 'Active', milestones: ['Public milestone'] }], reviews: [{ number: 4, title: 'Retain merged work', url: 'https://github.com/example/project/pull/4', lifecycle: 'merged', cli: [{ key: `public_round_${'b'.repeat(32)}`, found: 1, accepted: 1, routed: 0, complete: false, observed_at: '', duration_milliseconds: null }], hosted: [{ key: `public_round_${'c'.repeat(32)}`, found: 0, accepted: 0, routed: 0, complete: true, observed_at: '2026-10-17T11:00:00Z', duration_milliseconds: 30000 }] }] } }; }
afterEach(() => vi.unstubAllGlobals());
describe('public project browser', () => {
  it('selects anonymous routes before private session/profile or query-cache setup', () => {
    const storage = { getItem() { throw new Error('PRIVATE_SESSION_READ'); } }; vi.stubGlobal('sessionStorage', storage); vi.stubGlobal('localStorage', storage);
    for (const route of ['/public/showcase', `/public/showcase/history/2?generation=public_generation_${'a'.repeat(32)}`, '/public/INVALID', '/public/showcase/private-record']) {
      const html = renderToStaticMarkup(<MemoryRouter initialEntries={[route]}><App /></MemoryRouter>);
      expect(html).not.toContain('credential'); expect(html).not.toContain('Connect to a project'); expect(html).not.toContain('PRIVATE_SESSION_READ');
    }
  });
  it('renders approved content, retained merged reviews and separate honest channel rounds', () => {
    const html = renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={fixture()} /></MemoryRouter>);
    expect(html).toContain('Roadmap'); expect(html).toContain('Builder lane'); expect(html).toContain('Public milestone'); expect(html).toContain('Retain merged work'); expect(html).toContain('merged');
    expect(html).toContain('aria-label="Hosted review rounds"'); expect(html).toContain('aria-label="CLI review rounds"'); expect(html).toContain('Incomplete coverage'); expect(html).toContain('Time unknown'); expect(html).toContain('Duration unknown');
    expect(html).toContain('No progress summaries have been published'); expect(html).toContain('Powered by'); expect(html).toContain('flame'); expect(html).toContain('href="/"');
    expect(html).toContain('&lt;script&gt;literal approved text&lt;/script&gt;'); expect(html).not.toContain('<script>'); expect(html).not.toContain('iframe'); expect(html).not.toMatch(/project_id|actor_id|snapshot_id|review_evidence_|public_round_/u);
  });
  it('labels approved history as frozen and only links to public current view', () => {
    const html = renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={fixture()} historical /></MemoryRouter>);
    expect(html).toContain('Revision 2 · frozen'); expect(html).toContain('approved revision is frozen'); expect(html).toContain('href="/public/showcase"'); expect(html).not.toContain('/projects/'); expect(html).not.toContain('Live published view');
  });
});

it.each([
  { count: 0, hasMore: true, message: 'Finalized rounds are retained privately, but none fit this public window.' },
  { count: 0, hasMore: false, message: 'No finalized rounds published.' },
  { count: 1, hasMore: true, message: 'Showing 1 published round; other finalized rounds are outside this public window.' },
  { count: 2, hasMore: true, message: 'Showing 2 published rounds; other finalized rounds are outside this public window.' },
])('describes $count public CLI rounds with retained history $hasMore honestly', ({count, hasMore, message}) => {
  const snapshot=fixture();
  snapshot.content.reviews[0].cli=Array.from({length:count},(_,index)=>({...snapshot.content.reviews[0].cli[0],key:`public_round_${index.toString(16).padStart(32,'0')}`}));
  if(hasMore) snapshot.content.reviews[0].cli_has_more=true;
  else delete snapshot.content.reviews[0].cli_has_more;
  const html=renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={snapshot} /></MemoryRouter>);
  const lane=html.split('aria-label="CLI review rounds"')[1].split('</section>')[0];
  expect(lane).toContain(message);
  expect(lane).toContain(`${count} published ${count === 1 ? 'round' : 'rounds'}`);
  if(hasMore) expect(lane).not.toContain('No finalized rounds published.');
  else expect(lane).not.toContain('retained privately');
  if(!count) expect(lane).not.toContain('Showing 0');
  else { expect(lane).toContain('1 / 1 / 0'); expect(lane).not.toContain('Showing 1 published rounds'); }
  expect(html).not.toContain('/attempts');
});
