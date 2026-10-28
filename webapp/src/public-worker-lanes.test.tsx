import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import type { PublicLane } from './public-site-api';
import { PublicWorkerLanes } from './public-worker-lanes';

const lane: PublicLane = { key: 'approved_builder', label: 'Builder', headline: 'Ship the approved release', status: 'active', milestones: ['Ship the approved release', 'Publish the guide'] };

describe('approved public worker lanes', () => {
  it('uses the shared lane and job-card layout without private context, reads or links', () => {
    const fetch = vi.spyOn(globalThis, 'fetch');
    try {
      const supplied = { ...lane, worker_id: 'PRIVATE_WORKER', primary_job_id: 'PRIVATE_JOB', pause_reason: 'PRIVATE_PAUSE', brief: 'PRIVATE_BRIEF', presence: 'PRIVATE_CONTACT' };
      const html = renderToStaticMarkup(<PublicWorkerLanes lanes={[supplied]} />);
      expect(html).toContain('class="delivery-lanes"');
      expect(html).toContain('class="panel delivery-lane" aria-label="Builder published work"');
      expect(html).toContain('<header class="delivery-lane-heading"><div class="delivery-worker-heading"><h3>Builder</h3></div></header>');
      expect(html).toContain('<ul class="delivery-job-list"><li><div class="delivery-job-heading"><div class="delivery-job-state"><span class="badge status-active">Active</span></div><h4>Ship the approved release</h4>');
      expect(html).toContain('class="badge status-active">Active</span>');
      expect(html).toContain('Publish the guide');
      expect(html).not.toMatch(/PRIVATE_|approved_builder|<a\b|<nav\b|worker-contact|Primary focus|No open jobs|inbox|Job history/u);
      expect(fetch).not.toHaveBeenCalled();
    } finally { fetch.mockRestore(); }
  });

  it('keeps approved order and distinct milestones without repeating the headline or changing input', () => {
    const input = [{ ...lane, milestones: ['Ship the approved release', ' Publish the guide ', 'Publish the guide', 'Ship the approved release ', 'Second milestone'] }, { ...lane, key: 'another', label: 'Another lane', headline: 'Another approved headline', milestones: [] }];
    const before = structuredClone(input);
    const html = renderToStaticMarkup(<PublicWorkerLanes lanes={input} />);
    expect(html.match(/Ship the approved release/gu)).toHaveLength(1);
    expect(html.match(/Publish the guide/gu)).toHaveLength(1);
    expect(html.indexOf('Publish the guide')).toBeLessThan(html.indexOf('Second milestone'));
    expect(html.indexOf('Second milestone')).toBeLessThan(html.indexOf('Another lane'));
    expect(input).toEqual(before);
  });

  it.each(['active', 'blocked', 'parked', 'completed'])('uses the corresponding shared badge for approved %s status', status => {
    const html = renderToStaticMarkup(<PublicWorkerLanes lanes={[{ ...lane, status: status.toUpperCase() }]} />);
    expect(html).toContain(`class="badge status-${status}">${status[0].toUpperCase() + status.slice(1)}</span>`);
  });

  it('keeps unfamiliar approved vocabulary neutral and all public text inert', () => {
    const html = renderToStaticMarkup(<PublicWorkerLanes lanes={[{ ...lane, label: '<script>label</script>', headline: '<img src="https://example.com/tracker">', status: 'Awaiting <approval>', milestones: ['[Guide](javascript:alert(1))'] }]} />);
    expect(html).toContain('class="badge">Awaiting &lt;approval&gt;</span>');
    expect(html).toContain('&lt;script&gt;label&lt;/script&gt;');
    expect(html).toContain('&lt;img src=&quot;https://example.com/tracker&quot;&gt;');
    expect(html).toContain('[Guide](javascript:alert(1))');
    expect(html).not.toMatch(/<script|<img|<a\b|status-Awaiting/u);
  });

  it('does not invent a job title when only approved milestones are available', () => {
    const html = renderToStaticMarkup(<PublicWorkerLanes lanes={[{ ...lane, headline: '', milestones: ['An approved milestone'] }]} />);
    expect(html).toContain('An approved milestone');
    expect(html).toContain('delivery-job-list');
    expect(html).not.toMatch(/<h4|Read job|Primary focus/u);
  });

  it('distinguishes absent publication details from absent private jobs or inferred presence', () => {
    const html = renderToStaticMarkup(<PublicWorkerLanes lanes={[{ ...lane, headline: '', status: 'Idle', milestones: [] }]} />);
    expect(html).toContain('<h3>Builder</h3>');
    expect(html).toContain('class="badge">Idle</span>');
    expect(html).toContain('No approved work details have been published.');
    expect(html).not.toMatch(/<li\b|<h4|No open jobs|No jobs|Client attached|Last contact|Online/u);
    expect(renderToStaticMarkup(<PublicWorkerLanes lanes={[]} />)).toBe('<p class="muted">No worker lanes have been published.</p>');
    expect(renderToStaticMarkup(<PublicWorkerLanes lanes={[{ ...lane, status: '' }]} />)).toContain('class="badge">Status not published</span>');
  });
});
