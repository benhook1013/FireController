import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { EditorialMarkdown } from './editorial-markdown';
import { ProjectEditorialEntry } from './project-map';
import type { ProjectMapEntry } from './types';

const base: ProjectMapEntry = {
  id: 'map_entry_0123456789abcdef0123456789abcdef',
  project_id: 'project_one',
  revision: 2,
  kind: 'section',
  title: 'Project direction',
  description: '',
  state: '',
  where: '',
  next: '',
  trigger: '',
  phases: [],
  links: [],
  archived: false,
  created_at: '2026-10-18T00:00:00Z',
  updated_at: '2026-10-18T00:00:00Z',
  position: 10,
};

describe('safe editorial Markdown', () => {
  it('renders CommonMark structure, only safe links, and image alt text without loading images', () => {
    const html = renderToStaticMarkup(<EditorialMarkdown>{`# A heading

Paragraph with **emphasis**, \`inline code\`, and https://example.com.

- First point
- [External guide](https://example.com/guide)
- [On this page](#phase-one)
- [Script](javascript:alert%281%29)
- [Relative](/private/path)
- ![Remote image](https://example.com/private.png)`}</EditorialMarkdown>);
    expect(html).toContain('<h4>A heading</h4>');
    expect(html).toContain('<strong>emphasis</strong>');
    expect(html).toContain('<code>inline code</code>');
    expect(html).toContain('class="editorial-list editorial-list-unordered"');
    expect(html).toContain('href="https://example.com/guide" target="_blank" rel="noreferrer noopener"');
    expect(html).toContain('href="#phase-one"');
    expect(html).toContain('Script</span>');
    expect(html).toContain('Relative</span>');
    expect(html).toContain('Image: Remote image');
    expect(html).not.toMatch(/href="javascript:|href="\/private|<img|src="https:\/\/example.com\/private|dangerouslySetInnerHTML/u);
  });

  it('does not turn raw HTML into executable elements', () => {
    const html = renderToStaticMarkup(<EditorialMarkdown>{'Before <script>window.compromised = true</script> after <img src="https://example.com/x" />'}</EditorialMarkdown>);
    expect(html).not.toMatch(/<script|<img/u);
    expect(html).toContain('&lt;script&gt;');
    expect(html).toContain('Before');
    expect(html).toContain('after');
  });

  it('keeps compact headings below card titles while sharing the same link and image safety', () => {
    const body = '# Goal\n\n## Plan\n\n### Detail\n\n#### Small\n\n**Keep bold inline** and [safe](https://example.com) [unsafe](javascript:alert%281%29).\n\n![private image](https://example.com/private.png)\n\n<img src="https://example.com/private.png">';
    const compact = renderToStaticMarkup(<EditorialMarkdown compact>{body}</EditorialMarkdown>);
    expect(compact).toContain('<h5>Goal</h5>');
    expect(compact).toContain('<h5>Plan</h5>');
    expect(compact).toContain('<h6>Detail</h6>');
    expect(compact).toContain('<h6>Small</h6>');
    expect(compact).toContain('<strong>Keep bold inline</strong>');
    expect(compact).toContain('href="https://example.com" target="_blank" rel="noreferrer noopener"');
    expect(compact).toContain('editorial-unsafe-link');
    expect(compact).toContain('Image: private image');
    expect(compact).toContain('&lt;img');
    expect(compact).not.toMatch(/<h[1-4]|<img|href="javascript:|<script/u);
    expect(renderToStaticMarkup(<EditorialMarkdown>{body}</EditorialMarkdown>)).toContain('<h4>Goal</h4>');
  });

  it.each([
    '![private](https://example.com/private.png',
    '[unsafe](javascript:alert%281%29',
    '```html\n<script>alert(1)</script>\n<img src="https://example.com/private.png">',
    '**unfinished <img src="https://example.com/private.png">',
  ])('leaves truncated compact Markdown inert: %s', body => {
    const html = renderToStaticMarkup(<EditorialMarkdown compact>{body + '…'}</EditorialMarkdown>);
    expect(html).toContain('…');
    expect(html).not.toMatch(/<img|<script|href="javascript:|src="https:/u);
  });

  it('composes new sections and existing workstreams without empty taxonomy labels', () => {
    const section = renderToStaticMarkup(<ProjectEditorialEntry entry={{ ...base, description: '## A useful section\n\nLong-form **context**.' , state: 'In progress', links: [{ label: 'Design notes', url: 'https://example.com/design' }] }} />);
    expect(section).toContain('class="project-editorial-entry section"');
    expect(section).toContain('<h4>A useful section</h4>');
    expect(section).toContain('<strong>context</strong>');
    expect(section).toContain('In progress');
    expect(section).toContain('Design notes');
    expect(section).not.toMatch(/Where it stands|Next<|Reconsider when|Phases/u);

    const workstream = renderToStaticMarkup(<ProjectEditorialEntry entry={{ ...base, kind: 'workstream', title: 'Release readiness', description: '## Current picture\n\nEvidence is **improving**.', where: 'Signals are converging.', next: 'Recheck the smoke run.', phases: [{ id: 'proof', name: 'Prove release', state: 'Waiting on the test build', archived: false }] }} />);
    expect(workstream).toContain('Where it stands');
    expect(workstream).toContain('Next');
    expect(workstream).toContain('Prove release');
    expect(workstream).toContain('Waiting on the test build');
    expect(workstream).not.toContain('No state supplied');
  });

  it('renders return-point prose only when present', () => {
    const empty = renderToStaticMarkup(<ProjectEditorialEntry entry={{ ...base, kind: 'return_point' }} />);
    expect(empty).not.toMatch(/Description|Reconsider when|Links|No trigger supplied/u);
    const populated = renderToStaticMarkup(<ProjectEditorialEntry entry={{ ...base, kind: 'return_point', trigger: 'When the dependency ships, revisit **the estimate**.' }} />);
    expect(populated).toContain('Reconsider when');
    expect(populated).toContain('<strong>the estimate</strong>');
  });
});
