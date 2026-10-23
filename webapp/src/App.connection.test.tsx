import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryRouter, RouterProvider } from 'react-router';
import App, { OpeningProject } from './App';
import { Api } from './api';
import { browserSessionKey, browserRecoveryKey, type BrowserSessionBinding } from './browser-session';

const binding: BrowserSessionBinding = {
  version: 1, projectID: 'project_one', instanceID: 'instance_one', sessionID: 'a'.repeat(32),
  expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(), selectedworkerID: 'worker_one',
};
function storage(value: string | null) {
  const getItem = vi.fn((key: string) => key === browserSessionKey ? value : null);
  const setItem = vi.fn(), removeItem = vi.fn();
  vi.stubGlobal('sessionStorage', { getItem, setItem, removeItem });
  return { getItem, setItem, removeItem };
}
function firstPaint(path = '/projects/project_one/jobs/job_one') {
  const queries = new QueryClient();
  queries.setQueryData(['fc', 'prior_connection', 'instance_one', 'project_one', 'worker_one', 'jobs'], { items: [{ title: 'CACHED_PRIVATE_JOB' }] });
  const router = createMemoryRouter([{ path: '*', element: <App /> }], { initialEntries: [path] });
  try { return renderToStaticMarkup(<QueryClientProvider client={queries}><RouterProvider router={router} /></QueryClientProvider>); }
  finally { router.dispose(); queries.clear(); }
}
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('private connection first paint', () => {
  it('shows the restoring shell for a recreated tab with only durable project metadata', () => {
    storage(null);
    const setItem = vi.fn(), removeItem = vi.fn();
    vi.stubGlobal('localStorage', {getItem: (key: string) => key === browserRecoveryKey ? JSON.stringify([binding]) : null, setItem, removeItem});
    expect(firstPaint()).toContain('workspace-skeleton');
    expect(firstPaint()).not.toContain('Project credential');
    expect(firstPaint('/projects/project_other/jobs')).toContain('Project credential');
    expect(setItem).not.toHaveBeenCalled();
    expect(removeItem).not.toHaveBeenCalled();
  });
  it('renders a neutral stable skeleton before effects can resume saved access', () => {
    const saved = storage(JSON.stringify(binding));
    const resume = vi.spyOn(Api, 'resume');
    const connect = vi.spyOn(Api.prototype, 'connect');
    const html = firstPaint();
    expect(html).toContain('workspace-skeleton');
    expect(html).toContain('skeleton-hero');
    expect(html).toContain('page-skeleton');
    expect(html).not.toContain('Loading…');
    expect(html).toContain('role="status"><span class="visually-hidden">Opening project</span>');
    expect(html).not.toContain('Opening project…');
    expect(html).not.toContain('Verifying this tab’s saved connection.');
    expect(html).not.toContain('<h1');
    expect(html).not.toContain('<button');
    expect(html).not.toContain('opening-project-card');
    expect(html).not.toContain('class="connection-note"');
    expect(html).toContain('class="mast-label">Project workspace</span>');
    expect(html).not.toContain('Connect to a project');
    expect(html).not.toContain('Project credential');
    expect(html).not.toContain('<form');
    expect(html).not.toContain('CACHED_PRIVATE_JOB');
    expect(html).not.toContain(binding.projectID);
    expect(html).not.toContain(binding.selectedworkerID);
    expect(resume).not.toHaveBeenCalled();
    expect(connect).not.toHaveBeenCalled();
    // Initial render reads only presence. Validation/removal/logout belong to
    // the cancellable effect, so StrictMode render cannot expire a session.
    expect(saved.setItem).not.toHaveBeenCalled();
    expect(saved.removeItem).not.toHaveBeenCalled();
  });
  it('does not infer terminal expiry from an informational saved timestamp', () => {
    storage(JSON.stringify({ ...binding, expiresAt: '2025-01-01T00:00:00Z' }));
    const html = firstPaint();
    expect(html).toContain('<span class="visually-hidden">Opening project</span>');
    expect(html).not.toContain('Project credential');
  });
  it('shows the login form immediately for a fresh private destination without saved metadata', () => {
    storage(null);
    const html = firstPaint('/projects/project_one/jobs');
    expect(html).toContain('Sign in to project_one');
    expect(html).toContain('Project credential');
    expect(html).toContain('<form');
    expect(html).not.toContain('<span class="visually-hidden">Opening project</span>');
    expect(html).not.toContain('CACHED_PRIVATE_JOB');
  });
  it('keeps fresh login available if browser storage cannot be inspected', () => {
    vi.stubGlobal('sessionStorage', { getItem: () => { throw new Error('storage denied'); } });
    expect(firstPaint('/')).toContain('Connect to a project');
  });
  it('keeps transient resume failure separate from credential login', () => {
    const html = renderToStaticMarkup(<OpeningProject busy={false} notice="The service could not be reached in time. Try again when the connection is available." retry={() => {}} cancel={() => {}} />);
    expect(html).toContain('Project connection unavailable');
    expect(html).toContain('role="alert"');
    expect(html).toContain('The service could not be reached in time. Try again when the connection is available.');
    expect(html).not.toContain('loading-spinner');
    expect(html).toContain('Retry connection');
    expect(html).toContain('Cancel connection');
    expect(html).not.toContain('Project credential');
    expect(html).not.toContain('<form');
    expect(html).not.toContain('Connect to a project');
  });
});
