import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter, createMemoryRouter, RouterProvider } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import App, { ProjectPage, Workspace } from './App';
import { WorkspaceContext, type Session } from './workspace';
import type { ProjectMapEntry } from './types';

vi.mock('./public-site', async original => ({ ...await original<typeof import('./public-site')>(), PublicProjectAddress: ({ address }: { address: string }) => <div data-public-address={address}>APPROVED_PUBLIC_CONTENT</div> }));
vi.mock('./delivery', async original => ({ ...await original<typeof import('./delivery')>(), DeliveryHero: ({ children }: { children: React.ReactNode }) => <div data-private-hero>{children}</div>, DeliveryWork: () => <div data-shared-work>Current workers and jobs</div> }));
vi.mock('./review-workspace', async original => ({ ...await original<typeof import('./review-workspace')>(), ReviewQueueView: () => <div data-shared-reviews>Current review queue</div> }));
const home = '/projects/example';
const session = { nonce: 'connection', instanceId: 'instance', project: { id: 'project_one', handle: 'example', name: 'Example', repository: 'owner/example', github_repository_id: 1 }, controller: new AbortController(), api: { browserCapabilities: { can_edit_presentation: false } }, actions: { unresolved: 0, subscribe: () => () => {} } } as unknown as Session;
const key = ['fc', session.nonce, session.instanceId, session.project.id, ''];
const entry: ProjectMapEntry = { id: 'entry_current', project_id: 'project_one', kind: 'section', title: 'Current working section', description: 'PRIVATE_WORKING_CONTENT', state: '', where: '', next: '', trigger: '', phases: [], links: [], archived: false, revision: 3, created_at: '', updated_at: '' };
function render(path: string, workspace = false) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData([...key, 'map-editorial'], { pages: [{ items: [entry], has_more: false }], pageParams: [undefined] });
  try { return renderToStaticMarkup(<MemoryRouter initialEntries={[path]}><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, ready: true, selectWorker: () => {}, fail: () => {} }}>{workspace ? <Workspace session={session} metadataStatus="live" reconnectProjectUpdates={() => {}} selectWorker={() => {}} fail={() => {}} /> : <ProjectPage />}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>); }
  finally { client.clear(); }
}

describe('one Project view with separate publication authority', () => {
  it('shows current entries without duplicating Delivery and Reviews', () => {
    const html = render(`${home}/project`);
    expect(html).toContain('PRIVATE_WORKING_CONTENT'); expect(html).toContain('Private version');
    expect(html).toContain('View public version'); expect(html).toContain('href="/projects/example/project?view=public"');
    expect(html).not.toContain('data-shared-work'); expect(html).not.toContain('data-shared-reviews'); expect(html).not.toContain('APPROVED_PUBLIC_CONTENT');
  });
  it('selects approved content for preview without rendering current private entries or work', () => {
    const html = render(`${home}/project?view=public`, true);
    expect(html).toContain('APPROVED_PUBLIC_CONTENT'); expect(html).toContain('Public version'); expect(html).toContain('Back to private site');
    expect(html).not.toContain('PRIVATE_WORKING_CONTENT'); expect(html).not.toContain('data-private-hero'); expect(html).not.toContain('data-shared-work'); expect(html).not.toContain('data-shared-reviews');
    const navigation = html.match(/<nav class="navigation"[^>]*>(.*?)<\/nav>/u)![1];
    expect(navigation.match(/href="\/projects\/example\/project"/gu)).toHaveLength(1); expect(navigation).not.toContain('/map'); expect(navigation).not.toContain('Project map');
  });
  it('keeps anonymous Project requests on the approved content path even when private entries are cached', () => {
    const client = new QueryClient(); client.setQueryData([...key, 'map-editorial'], { pages: [{ items: [entry], has_more: false }], pageParams: [undefined] });
    vi.stubGlobal('sessionStorage', { getItem: () => null });
    const router = createMemoryRouter([{ path: '*', element: <App /> }], { initialEntries: [`${home}/project`] });
    try {
      const html = renderToStaticMarkup(<QueryClientProvider client={client}><RouterProvider router={router} /></QueryClientProvider>);
      expect(html).toContain('APPROVED_PUBLIC_CONTENT'); expect(html).not.toContain('PRIVATE_WORKING_CONTENT'); expect(html).not.toContain('data-shared-work'); expect(html).not.toContain('Working version');
    } finally { router.dispose(); client.clear(); vi.unstubAllGlobals(); }
  });
});
