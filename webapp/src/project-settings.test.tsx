import { describe, expect, it } from 'vitest';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderToStaticMarkup } from 'react-dom/server';
import { createMemoryRouter, RouterProvider } from 'react-router';
import App, { leavesActionProject } from './App';
import { decodePresentationDraft, presentationDraft } from './project-settings';

describe('explicit browser connection and appearance input', () => {
  it('defaults new connections to unchecked write consent without storing credentials or capabilities', () => {
    const router = createMemoryRouter([{ path: '*', element: <App /> }]);
    const html = renderToStaticMarkup(<QueryClientProvider client={new QueryClient()}><RouterProvider router={router} /></QueryClientProvider>);
    expect(html).toContain('Enable message and appearance changes'); expect(html).not.toContain('Read-only by default'); expect(html).toMatch(/type="checkbox"/u); expect(html).not.toMatch(/type="checkbox"[^>]*checked/u); expect(html).not.toContain('Save appearance');
  });
  it('guards exits to another project, root and public routes while allowing same-project view changes', () => {
    for (const pathname of ['/', '/projects/project_two/inbox', '/public/showcase', '/public']) expect(leavesActionProject('project_one', pathname)).toBe(true);
    for (const pathname of ['/projects/project_one', '/projects/project_one/inbox', '/projects/project_one/inbox/conversation_one', '/projects/project_one/settings']) expect(leavesActionProject('project_one', pathname)).toBe(false);
  });
  it('round-trips complete settings with literal headings and default theme reset', () => {
    const config = { name: 'Project <literal>', brand_name: 'Project brand', tagline: 'Project workspace', headings: { hero_title: '<script>Literal text</script>' }, theme: {} };
    const draft = presentationDraft(config);
    expect(decodePresentationDraft(draft)).toEqual(config);
    expect(decodePresentationDraft({ ...draft, headings: '{}', theme: '{}' })).toMatchObject({ headings: {}, theme: {} });
  });
  it('rejects unknown fields, scalar/null objects, executable CSS, invalid contrast and oversized input locally', () => {
    const valid = presentationDraft({ name: 'Project', brand_name: 'Brand', tagline: 'Workspace', headings: {}, theme: {} });
    for (const change of [{ headings: 'null' }, { headings: '[]' }, { headings: '{"unknown":"heading"}' }, { theme: '{"canvas_text":"url(javascript:alert(1))"}' }, { theme: '{"canvas_text":"#e8e7e4"}' }, { theme: ' '.repeat((16 << 10) + 1) }, { name: '' }]) expect(() => decodePresentationDraft({ ...valid, ...change })).toThrow();
  });
});
