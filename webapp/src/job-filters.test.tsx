import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Jobs } from './App';
import { jobFilterSearch, readJobFilters } from './job-navigation';
import { WorkspaceContext, type Session } from './workspace';

function markup(search: string) {
  const client = new QueryClient();
  const key = ['fc', 'connection_one', 'instance_one', 'project_one', 'inbox_actor'] as const;
  client.setQueryData([...key, 'workers', 'cached_workers_page'], { items: [{ id: 'worker_one', project_id: 'project_one', name: 'Selected worker' }], has_more: false });
  client.setQueryData([...key, 'workers', ''], { items: [], has_more: false });
  const job = { id: 'job_done', title: 'Finished selected job', worker_id: 'worker_one', status: 'completed', revision: 2 };
  const filters = readJobFilters(new URLSearchParams(search));
  client.setQueryData([...key, 'jobs', filters.status, filters.workerID, ''], { items: filters.status === 'open' || filters.workerID && filters.workerID !== job.worker_id ? [] : [job], has_more: false });
  const session = { worker: { id: 'inbox_actor', name: 'Inbox actor' }, controller: new AbortController(), api: {} } as Session;
  try {
    return renderToStaticMarkup(<MemoryRouter initialEntries={['/projects/project_one/jobs' + search]}><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project_one', selectWorker: () => {}, fail: () => {}, ready: false }}><Jobs /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
  } finally { client.clear(); }
}

describe('explicit job filter navigation', () => {
  it('honors a worker history link with all statuses, independently of the inbox actor', () => {
    const html = markup('?worker_id=worker_one&status=all');
    expect(html).toContain('<option value="" selected="">All statuses</option>');
    expect(html).toContain('<option value="worker_one" selected="">Selected worker</option>');
    expect(html).toContain('Finished selected job');
    expect(html).toContain('aria-label="Show jobs for all workers"');
    expect(html).not.toContain('Inbox actor');
  });
  it('defaults to open whole-project jobs with no inherited inbox filter', () => {
    expect(readJobFilters(new URLSearchParams())).toEqual({ status: 'open', workerID: '' });
    const html = markup('');
    expect(html).toContain('<option value="open" selected="">Open jobs</option>');
    expect(html).toContain('<option value="" selected="">All workers</option>');
    expect(html).not.toContain('Finished selected job');
  });
  it('serializes deliberate changes and clearing with only validated job selectors', () => {
    const initial = new URLSearchParams('worker_id=worker_one&status=all&view=compact');
    const filters = readJobFilters(initial);
    expect(readJobFilters(new URLSearchParams(jobFilterSearch({ ...filters, status: 'completed' })))).toEqual({ status: 'completed', workerID: 'worker_one' });
    const cleared = new URLSearchParams(jobFilterSearch({ ...filters, workerID: '' }));
    expect(readJobFilters(cleared)).toEqual({ status: '', workerID: '' });
    expect(cleared.get('view')).toBeNull();
    expect(initial.get('worker_id')).toBe('worker_one');
    expect(readJobFilters(new URLSearchParams(jobFilterSearch({ status: 'open', workerID: 'worker_two' })))).toEqual({ status: 'open', workerID: 'worker_two' });
  });
  it('defaults invalid or repeated selectors independently without inheriting the inbox actor', () => {
    for (const [query, status, workerID] of [
      ['status=', 'open', ''], ['status=bad&worker_id=worker_one', 'open', 'worker_one'],
      ['status=all&status=completed&worker_id=worker_one', 'open', 'worker_one'],
      ['status=all&worker_id=', '', ''], ['status=completed&worker_id=../worker', 'completed', ''],
      ['worker_id=worker_one&worker_id=worker_two', 'open', ''],
    ]) {
      expect(readJobFilters(new URLSearchParams(query))).toEqual({ status, workerID });
      const html = markup('?' + query);
      expect(html).toContain(`<option value="${status}" selected="">`);
      expect(html).toContain(`<option value="${workerID}" selected="">`);
      expect(html).not.toContain('The job filters are invalid.');
      expect(html).not.toContain('Inbox actor');
    }
  });
  it('displays an unresolved explicit worker selector with a clear action without broadening jobs', () => {
    const html = markup('?worker_id=worker_not_loaded&status=all');
    expect(html).toContain('<option value="worker_not_loaded" selected="">worker_not_loaded</option>');
    expect(html).toContain('aria-label="Show jobs for all workers"');
    expect(html).not.toContain('Finished selected job');
  });
});
