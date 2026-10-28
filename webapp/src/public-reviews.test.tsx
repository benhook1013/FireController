import { afterEach, describe, expect, it, vi } from 'vitest';
import { Children, isValidElement, type ReactNode } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { PublicReviews } from './public-reviews';
import type { PublicReview, PublicRound } from './public-site-api';

const round = (number: number, changes: Partial<PublicRound> = {}): PublicRound => ({ key: `public_round_${String(number).padStart(32, '0')}`, found: number, accepted: 0, routed: 1, complete: true, observed_at: '2026-10-21T12:00:00Z', duration_milliseconds: 1500, ...changes });
const review = (changes: Partial<PublicReview> = {}): PublicReview => ({ number: 7, title: '<literal approved title>', url: 'https://github.com/example/project/pull/7', lifecycle: 'merged', hosted: [], cli: [], ...changes });
const channel = (html: string, title: string) => html.split(`aria-label="${title} review rounds"`)[1]?.split('</section>')[0] ?? '';
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('allowlisted public reviews through shared normal presentation', () => {
  it('renders without private workspace/session/query providers or extra reads', () => {
    const fetch = vi.fn(() => { throw new Error('unexpected read'); }); vi.stubGlobal('fetch', fetch);
    const storage = { getItem() { throw new Error('private storage access'); } }; vi.stubGlobal('sessionStorage', storage); vi.stubGlobal('localStorage', storage);
    const html = renderToStaticMarkup(<PublicReviews reviews={[review({ hosted: [round(5)] })]} />);
    expect(fetch).not.toHaveBeenCalled(); expect(html).toContain('review-queue-row review-row-merged');
    expect(html).toContain('review-queue-position'); expect(html).toContain('Published order 01 on this page');
    expect(html).toContain('review-pull-facts"><span class="badge review-state-merged">Merged</span>');
    expect(html).toContain('class="review-history"'); expect(html).toContain('review-lanes');
    expect(html).toContain('&lt;literal approved title&gt;'); expect(html).not.toContain('<literal approved title>');
    const destinations = [...html.matchAll(/href="([^"]+)"/gu)].map(match => match[1]);
    expect(destinations.length).toBeGreaterThan(1); expect(new Set(destinations)).toEqual(new Set(['https://github.com/example/project/pull/7']));
    expect(html).not.toMatch(/CI:|Mergeability:|Current .*round limits|Review front|review-row-front|Review details|Review history|\/reviews\/|snapshot_id|project_id|review_evidence_|public_round_/u);
  });
  it('caps each channel at six in canonical producer order without sorting wall clocks or mutating inputs', () => {
    const hosted = Array.from({ length: 8 }, (_, index) => round(index + 1, { observed_at: index % 2 ? '' : `2026-10-${String(28 - index).padStart(2, '0')}T12:00:00Z` }));
    const cli = [round(21), round(22)], input = review({ hosted, cli }); const original = JSON.stringify(input);
    const html = renderToStaticMarkup(<PublicReviews reviews={[input]} />);
    const h = channel(html, 'Hosted'), c = channel(html, 'CLI');
    expect([...h.matchAll(/<strong>(\d+\/\d+\/\d+)<\/strong>/gu)].map(match => match[1])).toEqual(['3/0/1', '4/0/1', '5/0/1', '6/0/1', '7/0/1', '8/0/1']);
    expect([...c.matchAll(/<strong>(\d+\/\d+\/\d+)<\/strong>/gu)].map(match => match[1])).toEqual(['21/0/1', '22/0/1']);
    expect(h).toContain('8 published rounds'); expect(c).toContain('2 published rounds');
    expect(h).toContain('More published Hosted rounds are outside this view.'); expect(c).not.toContain('outside this view');
    expect(html).not.toMatch(/Round \d|all rounds|complete archive/u); expect(JSON.stringify(input)).toBe(original);
  });
  it('uses relative ages and approved completeness/counts without fabricating private outcomes or merge times', () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date('2026-10-21T14:05:00Z'));
    const html = renderToStaticMarkup(<PublicReviews reviews={[review({ hosted: [round(5)], cli: [round(6, { complete: false, observed_at: '', duration_milliseconds: null })] })]} />);
    expect(channel(html, 'Hosted')).toContain('review-pill-completed review-pill-zero');
    expect(channel(html, 'Hosted')).toContain('<strong>5/0/1</strong>'); expect(html).toContain('>2h 5m ago</time>');
    expect(html).toContain('dateTime="2026-10-21T12:00:00Z"'); expect(html).toContain('1.5 seconds');
    expect(channel(html, 'CLI')).not.toContain('review-pill-zero'); expect(channel(html, 'CLI')).toContain('Incomplete coverage');
    expect(html).toContain('Time unknown'); expect(html).toContain('Duration unknown');
    expect(html).not.toContain('review-merged-age'); expect(html).not.toContain('>Completed<'); expect(html).not.toContain('fixed');
  });
  it('does not turn an absent bounded channel into an absent-history or coverage claim', () => {
    const html = renderToStaticMarkup(<PublicReviews reviews={[review({ hosted_has_more: true })]} />);
    expect(channel(html, 'Hosted')).toContain('No finalized rounds fit in this view.');
    expect(channel(html, 'Hosted')).toContain('More published Hosted rounds are outside this view.');
    expect(channel(html, 'CLI')).toContain('No finalized rounds published.');
    expect(html).toContain('href="#published-history"'); expect(html).toContain('these snapshots may not contain every round');
    expect(html).not.toContain('Time unknown');
  });
  it('uses only the deliberate existing public history continuation action', () => {
    const browse = vi.fn(); let found = false;
    function visit(node: ReactNode): void {
      Children.forEach(node, child => {
        if (found || !isValidElement<Record<string, unknown>>(child)) return;
        if (child.type === 'a' && child.props.href === '#published-history' && typeof child.props.onClick === 'function') { child.props.onClick(); found = true; }
        else if (typeof child.type === 'function' && child.type.name !== 'RelativeTime') visit((child.type as (props: Record<string, unknown>) => ReactNode)(child.props));
        else visit(child.props.children as ReactNode);
      });
    }
    visit(<PublicReviews reviews={[review({ cli_has_more: true })]} browseHistory={browse} />);
    expect(browse).toHaveBeenCalledOnce();
  });
  it('keeps lifecycle unknown and refuses unintended internal or unsafe link destinations', () => {
    for (const url of ['/projects/private/reviews/runs/id', 'javascript:alert(1)', 'https://github.com/example/project/pull/8', 'https://github.com/example/project/pull/7#private']) {
      const html = renderToStaticMarkup(<PublicReviews reviews={[review({ url, lifecycle: 'unknown', cli: [round(2)] })]} />);
      expect(html).toContain('Lifecycle unknown'); expect(html).not.toContain('href=');
    }
    expect(renderToStaticMarkup(<PublicReviews reviews={[]} />)).toContain('No pull requests have been selected for publication.');
  });
});
