import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter, Link } from 'react-router';
import { ReviewLaneDisplay, ReviewRoundPillDisplay, ReviewRowDisplay } from './review-display';

describe('shared review presentation shells', () => {
  it('renders the normal queue rail/title and supplied facts without inventing a front or observations', () => {
    const html = renderToStaticMarkup(<ReviewRowDisplay id="review-pr-7" lifecycle="merged" position="02" positionLabel="Published order 02 on this page" number={7} title={'<literal title>'} url="https://github.com/example/project/pull/7" facts={<div className="review-pull-facts">Supplied facts</div>} footer={<a href="#published-history">History</a>}><p>Supplied lanes</p></ReviewRowDisplay>);
    expect(html).toContain('id="review-pr-7"'); expect(html).toContain('review-queue-row review-row-merged');
    expect(html).toContain('class="review-queue-position" aria-label="Published order 02 on this page">02');
    expect(html).toContain('class="review-queue-content"'); expect(html).toContain('class="review-pull-heading"');
    expect(html).toContain('&lt;literal title&gt;'); expect(html).not.toContain('<literal title>');
    expect(html.match(/href="https:\/\/github.com\/example\/project\/pull\/7"/gu)).toHaveLength(2);
    expect(html).toContain('target="_blank" rel="noopener noreferrer"');
    expect(html.indexOf('Supplied facts')).toBeLessThan(html.indexOf('Supplied lanes'));
    expect(html.indexOf('Supplied lanes')).toBeLessThan(html.indexOf('>History</a>'));
    expect(html).not.toMatch(/review-row-front|CI:|Mergeability:|snapshot_id/u);
  });
  it('can omit the rail without inventing an order or changing the shared body', () => {
    const html = renderToStaticMarkup(<ReviewRowDisplay rail={false} lifecycle="open" number={7} title="Shared heading"><p>Shared lanes</p></ReviewRowDisplay>);
    expect(html).toContain('review-row-without-order');
    expect(html).not.toContain('review-queue-position');
    expect(html).not.toContain('Stored order');
    expect(html).toContain('class="review-pull-heading"');
    expect(html).toContain('<h3>Shared heading</h3>');
  });
  it('keeps caller-supplied lane headers and private link labels/targets intact', () => {
    const html = renderToStaticMarkup(<MemoryRouter><ReviewLaneDisplay title="Hosted CodeRabbit" metadata="8 runs" metadataTitle="Retained review runs in this channel; not current-head coverage"><ReviewRoundPillDisplay found={3} accepted={0} routed={2} className="review-pill-completed semantic-change" zeroAccepted title="Original description" label="Open hosted run original: Original description" wrap={content => <Link {...content} to="/projects/selected/reviews/runs/run-one?snapshot=retained" />}><span className="review-pill-age">Date unknown</span></ReviewRoundPillDisplay></ReviewLaneDisplay></MemoryRouter>);
    expect(html).toContain('class="review-lane" aria-label="Hosted CodeRabbit"');
    expect(html).toContain('<h4>Hosted CodeRabbit</h4>'); expect(html).toContain('8 runs');
    expect(html).toContain('review-round-pill review-pill-completed semantic-change review-pill-zero');
    expect(html).toContain('<strong>3/0/2</strong>'); expect(html).toContain('aria-label="Open hosted run original: Original description"');
    expect(html).toContain('href="/projects/selected/reviews/runs/run-one?snapshot=retained"');
    expect(html).toContain('Date unknown');
  });
  it('supports unlinked supplied content without manufacturing navigation', () => {
    const html = renderToStaticMarkup(<ReviewRoundPillDisplay found={1} accepted={1} routed={0} title="Published observation" label="Published CLI round"><span>Supplied age</span></ReviewRoundPillDisplay>);
    expect(html).toContain('<span class="review-round-pill"'); expect(html).toContain('<strong>1/1/0</strong>');
    expect(html).not.toContain('href='); expect(html).not.toContain('review-pill-zero');
  });
});
