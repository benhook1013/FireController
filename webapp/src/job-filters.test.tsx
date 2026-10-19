import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Jobs, changedJobFilters, jobFilters } from './App';
import { WorkspaceContext, type Session } from './workspace';

function markup(search: string) {
  const client = new QueryClient();
  const key = ['fc', 'connection_one', 'instance_one', 'project_one', 'inbox_actor'] as const;
  client.setQueryData([...key, 'worker', 'worker_one'], { id: 'worker_one', project_id: 'project_one', name: 'Selected worker' });
  client.setQueryData([...key, 'workers', ''], { items: [], has_more: false });
  const job = { id: 'job_done', title: 'Finished selected job', worker_id: 'worker_one', status: 'completed', revision: 2 };
  client.setQueryData([...key, 'jobs', '', 'worker_one', ''], { items: [job], has_more: false });
  client.setQueryData([...key, 'jobs', 'open', '', ''], { items: [], has_more: false });
  const session = { worker: { id: 'inbox_actor', name: 'Inbox actor' }, controller: new AbortController(), api: {} } as Session;
  try {
    return renderToStaticMarkup(<MemoryRouter initialEntries={['/projects/project_one/jobs' + search]}><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project_one', selectWorker: () => {}, fail: () => {}, ready: false }}><Jobs /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
  } finally { client.clear(); }
}

describe('explicit job filter navigation', () => {
  it('honors a worker history link with all statuses, independently of the inbox actor', () => {
    const html = markup('?worker_id=worker_one&status=all');
    expect(html).toContain('<option value="all" selected="">All statuses</option>');
    expect(html).toContain('<option value="worker_one" selected="">Selected worker</option>');
    expect(html).toContain('Finished selected job');
    expect(html).toContain('aria-label="Show jobs for all workers"');
    expect(html).not.toContain('Inbox actor');
  });
  it('defaults to open whole-project jobs with no inherited inbox filter', () => {
    expect(jobFilters(new URLSearchParams())).toEqual({ status: 'open', workerId: '' });
    const html = markup('');
    expect(html).toContain('<option value="open" selected="">Open jobs</option>');
    expect(html).toContain('<option value="" selected="">All workers</option>');
    expect(html).not.toContain('Finished selected job');
  });
  it('keeps deliberate filter changes and clearing in the URL while preserving unrelated selectors', () => {
    const initial = new URLSearchParams('worker_id=worker_one&status=all&view=compact');
    expect(jobFilters(changedJobFilters(initial, { status: 'completed' }))).toEqual({ status: 'completed', workerId: 'worker_one' });
    const cleared = changedJobFilters(initial, { workerId: '' });
    expect(jobFilters(cleared)).toEqual({ status: 'all', workerId: '' });
    expect(cleared.get('view')).toBe('compact');
    expect(initial.get('worker_id')).toBe('worker_one');
    expect(jobFilters(changedJobFilters(cleared, { workerId: 'worker_two', status: 'open' }))).toEqual({ status: 'open', workerId: 'worker_two' });
  });
  it('refuses empty, invalid or repeated selectors rather than falling back to a broader job list', () => {
    for (const query of ['status=', 'status=bad', 'status=all&status=completed', 'worker_id=', 'worker_id=../worker', 'worker_id=worker_one&worker_id=worker_two']) {
      expect(jobFilters(new URLSearchParams(query))).toBeUndefined();
      const html = markup('?' + query);
      expect(html).toContain('The job filters are invalid.');
      expect(html).toContain('Reset job filters');
      expect(html).not.toContain('Finished selected job');
    }
  });
  it('displays an unresolved explicit worker selector with a clear action without broadening jobs', () => {
    const html = markup('?worker_id=worker_not_loaded&status=all');
    expect(html).toContain('<option value="worker_not_loaded" selected="">worker_not_loaded</option>');
    expect(html).toContain('aria-label="Show jobs for all workers"');
    expect(html).not.toContain('Finished selected job');
  });
});
