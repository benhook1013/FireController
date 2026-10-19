import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { JobWorkstreamPanel, MapEntryContent, MapSummaryRows, MapHistoryRows, MapRevisionContent, WorkstreamReferenceContent } from './project-map';
import type { ProjectMapEntry } from './types';

const entry: ProjectMapEntry = { id: 'entry_one', project_id: 'project_one', kind: 'workstream', title: '<script>Literal title</script>', state: 'Deferred', description: 'PRIVATE_DESCRIPTION <img src=x>', where: 'PRIVATE_WHERE', next: 'PRIVATE_NEXT', trigger: '', phases: [{ id: 'proof', name: 'Earlier <phase>', state: 'Earlier <state>', archived: true }], links: [{ label: 'Safe <link>', url: 'https://example.com/path?a=1&b=2' }, { label: 'Unsafe link', url: 'javascript:alert(1)' }], archived: true, revision: 4, created_at: '2026-10-16T00:00:00Z', updated_at: '2026-10-16T01:00:00Z' };
const home = '/projects/project_one';

describe('read-only project map components', () => {
  it('renders compact list rows with explicit entry links and no full bodies', () => {
    const html = renderToStaticMarkup(<MemoryRouter><MapSummaryRows rows={[entry]} home={home} /></MemoryRouter>);
    expect(html).toContain('Workstream · Archived');
    expect(html).toContain('Revision 4');
    expect(html).toContain('&lt;script&gt;Literal title&lt;/script&gt;');
    expect(html).toContain('href="/projects/project_one/map/entries/entry_one"');
    expect(html).not.toMatch(/PRIVATE_DESCRIPTION|PRIVATE_WHERE|PRIVATE_NEXT|Earlier|https:|<script>/u);
  });
  it('shows captured editorial content and phases literally and rejects executable links', () => {
    const html = renderToStaticMarkup(<MapEntryContent entry={entry} />);
    expect(html).toContain('&lt;img src=x&gt;');
    expect(html).toContain('Earlier &lt;phase&gt;');
    expect(html).toContain('Earlier &lt;state&gt;');
    expect(html).toContain('Next milestone');
    expect(html).toContain('target="_blank" rel="noreferrer noopener"');
    expect(html).toContain('Unsafe link (unavailable link)');
    expect(html).not.toMatch(/<script>|<img|href="javascript:|dangerouslySetInnerHTML/u);
  });
  it('renders an unavailable label for retained HTTP URLs that browsers reject', () => {
    for (const url of ['https://example.com:99999/x', 'https://<example.com/x', 'https://>example.com/x']) {
      const html = renderToStaticMarkup(<MapEntryContent entry={{ ...entry, links: [{ label: 'Retained reference', url }] }} />);
      expect(html).toContain('Retained reference (unavailable link)');
      expect(html).toContain('PRIVATE_DESCRIPTION');
      expect(html).not.toContain('href=');
    }
  });
  it('distinguishes return-point triggers from workstream milestones and completion claims', () => {
    const html = renderToStaticMarkup(<MapEntryContent entry={{ ...entry, kind: 'return_point', where: '', next: '', phases: [], trigger: 'When prerequisite <lands>', state: 'HUMAN DECISION', links: [] }} />);
    expect(html).toContain('Return point · Archived');
    expect(html).toContain('Reconsider when');
    expect(html).toContain('When prerequisite &lt;lands&gt;');
    expect(html).not.toMatch(/Next milestone|<h3>Phases|merge.ready|Completed/u);
  });
  it('labels the association title as recorded and does not load references until opened', () => {
    const closed = renderToStaticMarkup(<JobWorkstreamPanel jobId="job_one" />);
    expect(closed).toContain('aria-expanded="false"');
    expect(closed).toContain('Show workstream reference');
    expect(closed).not.toContain('Recorded title');
    const html = renderToStaticMarkup(<MemoryRouter><WorkstreamReferenceContent value={{ job_id: 'job_one', workstream_id: entry.id, workstream_title: 'Old <title>', revision: 2 }} home={home} /></MemoryRouter>);
    expect(html).toContain('Recorded title: Old &lt;title&gt;');
    expect(html).toContain('current workstream title or archive state may have changed');
    expect(html).toContain('Open current workstream');
    expect(html).toContain('Association revision 2');
    const empty = renderToStaticMarkup(<WorkstreamReferenceContent value={{ job_id: 'job_one', workstream_id: '', workstream_title: '', revision: 0 }} home={home} />);
    expect(empty).toContain('No workstream reference');
    expect(empty).not.toContain('href=');
  });
  it('keeps history summaries compact and labels a deliberately selected full snapshot', () => {
    const row = { revision: 4, actor_id: 'principal_one', created_at: entry.updated_at, state: entry };
    const summaries = renderToStaticMarkup(<MemoryRouter><MapHistoryRows rows={[row]} home={home} entryId={entry.id} /></MemoryRouter>);
    expect(summaries).toContain('href="/projects/project_one/map/entries/entry_one/history/4"');
    expect(summaries).toContain('Read revision 4');
    expect(summaries).not.toMatch(/PRIVATE_DESCRIPTION|Earlier|Literal title/u);
    const snapshot = renderToStaticMarkup(<MapRevisionContent value={row} />);
    expect(snapshot).toContain('Captured revision 4');
    expect(snapshot).toContain('does not describe current state');
    expect(snapshot).toContain('PRIVATE_DESCRIPTION');
    expect(snapshot).toContain('Recorded by principal_one');
  });
});
