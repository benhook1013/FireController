import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { Children, isValidElement, type ReactNode } from 'react';
import App from './App';
import { PublicSnapshotView } from './public-site';
import type { PublicSnapshot } from './public-site-api';

function fixture(): PublicSnapshot { const generation = `public_generation_${'a'.repeat(32)}`; return { slug: 'showcase', generation, revision: 2, published_at: '2026-10-17T12:00:00Z', cursor: `${generation}:2`, content: { slug: 'showcase', presentation: { name: 'Showcase', brand_name: 'Showcase', tagline: 'A project worth sharing', headings: { map: 'Roadmap' }, theme: {} }, description: '<script>literal approved text</script>', links: [{ key: 'source', label: 'Project source', url: 'https://example.com/project' }], project_map: [{ key: 'build', title: 'Build the project', description: 'Approved roadmap description', phase: 'Build', status: 'Active', links: [] }], progress: [], worker_lanes: [{ key: 'builder', label: 'Builder lane', headline: 'Shipping the project', status: 'Active', milestones: ['Public milestone'] }], reviews: [{ number: 4, title: 'Retain merged work', url: 'https://github.com/example/project/pull/4', lifecycle: 'merged', cli: [{ key: `public_round_${'b'.repeat(32)}`, found: 1, accepted: 1, routed: 0, complete: false, observed_at: '', duration_milliseconds: null }], hosted: [{ key: `public_round_${'c'.repeat(32)}`, found: 0, accepted: 0, routed: 0, complete: true, observed_at: '2026-10-17T11:00:00Z', duration_milliseconds: 30000 }] }] } }; }
afterEach(() => vi.unstubAllGlobals());
describe('public project browser', () => {
  it('renders absent and explicit false window flags identically', () => {
    const old = fixture(), before = JSON.stringify(old);
    const render = (value: PublicSnapshot) => renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={value} /></MemoryRouter>);
    const expected = render(old), flagged = fixture();
    flagged.content.reviews[0].cli_has_more = false; flagged.content.reviews[0].hosted_has_more = false;
    expect(render(flagged)).toBe(expected); expect(JSON.stringify(old)).toBe(before);
    expect(expected).toContain('<strong>0/0/0</strong>'); expect(expected).not.toContain('outside this view');
  });
  it('shows a bounded channel window without lifetime ordinals or complete-archive promises', () => {
    const value = fixture(), review = value.content.reviews[0];
    review.hosted = Array.from({ length: 100 }, (_, i) => ({ ...review.hosted[0], key: `public_round_${i.toString(16).padStart(32, '0')}` }));
    review.hosted_has_more = true;
    const html = renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={value} /></MemoryRouter>);
    const hosted = html.match(/<section class="review-lane" aria-label="Hosted review rounds">([\s\S]*?)<\/section>/u)![1];
    const cli = html.match(/<section class="review-lane" aria-label="CLI review rounds">([\s\S]*?)<\/section>/u)![1];
    expect(hosted).toContain('100 published rounds'); expect(hosted.match(/class="review-round-pill /gu)).toHaveLength(6);
    expect(hosted).toContain('More published Hosted rounds are outside this view');
    expect(hosted).toContain('href="#published-history"'); expect(html).toContain('id="published-history"');
    expect(hosted).toContain('these snapshots may not contain every round'); expect(hosted).not.toMatch(/>Round \d|all rounds|complete archive/iu);
    expect(cli).toContain('<strong>1/1/0</strong>'); expect(cli).not.toContain('outside this view');
    expect(html).toContain('merged'); expect(html).not.toMatch(/project_id|snapshot_id|review_evidence_/u);
  });
  it('connects the window notice to the existing revision browser action', () => {
    const value = fixture(); value.content.reviews[0].cli_has_more = true;
    const browse = vi.fn();
    function visit(node: ReactNode): void {
      Children.forEach(node, child => {
        if (!isValidElement<Record<string, unknown>>(child)) return;
        if (typeof child.type === 'function' && ['PublicReviews', 'ReviewRowDisplay', 'PublicReviewLane', 'ReviewLaneDisplay'].includes(child.type.name)) {
          visit((child.type as (props: Record<string, unknown>) => ReactNode)(child.props));
        } else if (child.type === 'a' && child.props.href === '#published-history' && typeof child.props.onClick === 'function') {
          child.props.onClick();
        } else visit(child.props.children as ReactNode);
      });
    }
    visit(PublicSnapshotView({ snapshot: value, browseHistory: browse }));
    expect(browse).toHaveBeenCalledOnce();
  });
  it('does not imply no published history when the byte window contains no rounds', () => {
    const value = fixture(); value.content.reviews[0].cli = []; value.content.reviews[0].cli_has_more = true;
    const html = renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={value} historical /></MemoryRouter>);
    expect(html).toContain('No finalized rounds fit in this view.'); expect(html).toContain('More published CLI rounds are outside this view.');
    expect(html).toContain('approved revision is frozen');
  });
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
    expect(html).not.toContain('No progress summaries have been published'); expect(html).not.toContain('href="#progress"'); expect(html).toContain('Powered by'); expect(html).toContain('flame'); expect(html).toContain('href="/projects/showcase?signin=1"');
    expect(html).toContain('&lt;script&gt;literal approved text&lt;/script&gt;'); expect(html).not.toContain('<script>'); expect(html).not.toContain('iframe'); expect(html).not.toMatch(/project_id|actor_id|snapshot_id|review_evidence_|public_round_/u);
  });
  it('keeps approved content visible while refreshing or reconnecting', () => {
    for (const status of ['connecting', 'reconnecting', 'closed']) {
      const html = renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={fixture()} status={status} refreshing refresh={() => {}} /></MemoryRouter>);
      expect(html).toContain('Approved roadmap description');
      expect(html).toContain('A project worth sharing');
      expect(html).toContain('Retain merged work');
      expect(html).not.toContain('public-project-skeleton');
      expect(html).not.toContain('Try again');
    }
  });
  it('labels approved history as frozen and only links to public current view', () => {
    const html = renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={fixture()} historical /></MemoryRouter>);
    expect(html).toContain('Revision 2 · frozen'); expect(html).toContain('approved revision is frozen'); expect(html).toContain('href="/projects/showcase"'); expect(html).not.toContain('/public/'); expect(html).not.toContain('Live published view');
  });
  it('renders approved Markdown as narrative without executing HTML or fetching authored images', () => {
    const value = fixture(); value.content.description = '## Purpose\n\nUse **agents** together. ![Private image](https://elsewhere.example/track.png)';
    value.content.project_map[0].description = 'A short *update*.\n\n- A milestone\n- [Reference](https://example.com/reference)';
    const html = renderToStaticMarkup(<MemoryRouter><PublicSnapshotView snapshot={value} /></MemoryRouter>);
    expect(html).toContain('<strong>agents</strong>'); expect(html).toContain('<em>update</em>'); expect(html).toContain('public-editorial'); expect(html).toContain('public-story');
    expect(html).toContain('href="https://example.com/reference"'); expect(html).toContain('rel="noreferrer noopener"');
    expect(html).not.toContain('src="https://elsewhere.example'); expect(html).not.toContain('private_extra'); expect(html).not.toContain('/projects/project_private');
  });

});
