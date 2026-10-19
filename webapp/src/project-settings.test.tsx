import { describe, expect, it } from 'vitest';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderToStaticMarkup } from 'react-dom/server';
import { createMemoryRouter, RouterProvider } from 'react-router';
import App, { leavesActionProject } from './App';
import { decodePresentationDraft, presentationDraft } from './project-settings';
import type { ReviewGroup } from './types';

describe('browser connection and appearance input', () => {
  it('offers one login without a permission-mode checkbox', () => {
    const router = createMemoryRouter([{ path: '*', element: <App /> }]);
    const html = renderToStaticMarkup(<QueryClientProvider client={new QueryClient()}><RouterProvider router={router} /></QueryClientProvider>);
    expect(html).toContain('Connect to a project'); expect(html).not.toMatch(/type="checkbox"/u); expect(html).not.toContain('Enable message and appearance changes'); expect(html).not.toContain('Save appearance');
  });
  it('guards exits to another project, root and public routes while allowing same-project view changes', () => {
    for (const pathname of ['/', '/projects/project_two/inbox', '/public/showcase', '/public']) expect(leavesActionProject('project_one', pathname)).toBe(true);
    for (const pathname of ['/projects/project_one', '/projects/project_one/inbox', '/projects/project_one/inbox/conversation_one', '/projects/project_one/settings']) expect(leavesActionProject('project_one', pathname)).toBe(false);
  });
  it('round-trips complete settings with literal headings and default theme reset', () => {
    const config = { name: 'Project <literal>', brand_name: 'Project brand', tagline: 'Project workspace', headings: { hero_title: '<script>Literal text</script>' }, theme: {} };
    const draft = presentationDraft(config);
    expect(decodePresentationDraft(draft)).toEqual(config);
    expect(Object.hasOwn(draft, 'review_groups')).toBe(false);
    expect(Object.hasOwn(decodePresentationDraft(draft), 'review_groups')).toBe(false);
    expect(decodePresentationDraft({ ...draft, headings: '{}', theme: '{}' })).toMatchObject({ headings: {}, theme: {} });
  });
  it('retains independently owned review groups through unrelated appearance edits', () => {
    const config = { name: 'Project', brand_name: 'Brand', tagline: 'Workspace', headings: {}, theme: {}, review_groups: [{ title: 'Foundation', pull_numbers: [1, 3] }] };
    const draft = presentationDraft(config);
    expect(decodePresentationDraft(draft)).toEqual(config);
    config.review_groups[0].title = 'Source changed'; config.review_groups[0].pull_numbers.push(99);
    expect(draft.review_groups).toEqual([{ title: 'Foundation', pull_numbers: [1, 3] }]);
    const decoded = decodePresentationDraft({ ...draft, name: 'Renamed', theme: '{"surface_text":"#000000"}' });
    expect(decoded).toMatchObject({ name: 'Renamed', theme: { surface_text: '#000000' }, review_groups: [{ title: 'Foundation', pull_numbers: [1, 3] }] });
    decoded.review_groups![0].title = 'Decoded changed'; decoded.review_groups![0].pull_numbers.push(7);
    expect(draft.review_groups).toEqual([{ title: 'Foundation', pull_numbers: [1, 3] }]);
    expect(config.review_groups).toEqual([{ title: 'Source changed', pull_numbers: [1, 3, 99] }]);
  });
  it('refuses malformed or duplicate preserved groups through common config validation', () => {
    const draft = presentationDraft({ name: 'Project', brand_name: 'Brand', tagline: 'Workspace', headings: {}, theme: {} });
    for (const groups of [null, [{ title: 'One', pull_numbers: [1, 1] }], [{ title: 'One', pull_numbers: [1] }, { title: 'Two', pull_numbers: [1] }], [{ title: 'One', pull_numbers: [0] }], [{ title: 'One', pull_numbers: [1], extra: true }]]) {
      expect(() => decodePresentationDraft({ ...draft, review_groups: groups as unknown as ReviewGroup[] })).toThrow('Use supported headings, colors, contrast and bounded text.');
    }
    expect(decodePresentationDraft({ ...draft, review_groups: [] }).review_groups).toEqual([]);
  });
  it('rejects unknown fields, scalar/null objects, executable CSS, invalid contrast and oversized input locally', () => {
    const valid = presentationDraft({ name: 'Project', brand_name: 'Brand', tagline: 'Workspace', headings: {}, theme: {} });
    for (const change of [{ headings: 'null' }, { headings: '[]' }, { headings: '{"unknown":"heading"}' }, { theme: '{"canvas_text":"url(javascript:alert(1))"}' }, { theme: '{"canvas_text":"#e8e7e4"}' }, { theme: ' '.repeat((16 << 10) + 1) }, { name: '' }]) expect(() => decodePresentationDraft({ ...valid, ...change })).toThrow();
  });
});
