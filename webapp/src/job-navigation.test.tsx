import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { JobDetail, Jobs } from './App';
import { jobFilterSearch, readJobFilters } from './job-navigation';
import { WorkspaceContext, type Session } from './workspace';

const parse = (query: string) => readJobFilters(new URLSearchParams(query));

describe('validated job navigation context', () => {
  it('distinguishes default open from explicit all and retains each supported status', () => {
    expect(parse('')).toEqual({ status: 'open', workerID: '' });
    expect(parse('status=all')).toEqual({ status: '', workerID: '' });
    expect(jobFilterSearch(parse('status=all'))).toBe('?status=all');
    for (const status of ['open', 'active', 'parked', 'blocked', 'completed']) {
      expect(parse(`status=${status}`).status).toBe(status);
      expect(parse(jobFilterSearch(parse(`status=${status}&worker_id=worker_one`)))).toEqual({ status, workerID: 'worker_one' });
    }
  });
  it('defaults malformed or repeated selectors independently and drops unrelated return data', () => {
    for (const query of ['status=', 'status=bogus', 'status=completed&status=active', 'status=all&status=all']) expect(parse(query).status).toBe('open');
    for (const worker of ['', '../other', 'worker/name', 'worker name', '-worker', 'a'.repeat(129), 'worker\u0000']) expect(parse(`worker_id=${encodeURIComponent(worker)}`).workerID).toBe('');
    expect(parse('worker_id=worker_one&worker_id=worker_one').workerID).toBe('');
    expect(parse('status=bogus&worker_id=worker_one')).toEqual({ status: 'open', workerID: 'worker_one' });
    expect(parse(`status=completed&worker_id=${'a'.repeat(128)}`).workerID).toHaveLength(128);
    expect(jobFilterSearch(parse('status=all&worker_id=worker_one&return_url=https://untrusted.test&cursor=private_cursor&snapshot=other'))).toBe('?status=all&worker_id=worker_one');
  });
});

function markup(query: string, detail = false) {
  const client = new QueryClient();
  const key = ['fc', 'connection_nav'];
  const filters = parse(query);
  client.setQueryData([...key, 'workers', ''], { items: [], has_more: true, cursor: 'next_workers' });
  client.setQueryData([...key, 'workers', 'next_workers'], { items: [{ id: 'worker_one', name: 'Real cached worker label', project_id: 'project_nav' }], has_more: false });
  client.setQueryData([...key, 'jobs', filters.status, filters.workerID, ''], { items: [{ id: 'job_one', title: 'Focused job', status: 'completed', worker_id: 'worker_one', revision: 1 }], has_more: false });
  const session = { worker: { id: 'inbox_other', name: 'Inbox actor' } } as Session;
  try {
    return renderToStaticMarkup(<QueryClientProvider client={client}><MemoryRouter initialEntries={[`/projects/project_nav/jobs${detail ? '/job_one' : ''}?${query}`]}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project_nav', selectWorker: () => {}, fail: () => {}, ready: false }}>{detail ? <JobDetail /> : <Jobs />}</WorkspaceContext.Provider></MemoryRouter></QueryClientProvider>);
  } finally { client.clear(); }
}

describe('job list and return links', () => {
  it('renders matching URL filters, cached server worker labels and both scoped detail links', () => {
    const html = markup('status=completed&worker_id=worker_one&return_url=https://untrusted.test');
    expect(html).toContain('<option value="completed" selected="">Completed</option>');
    expect(html).toContain('<option value="worker_one" selected="">Real cached worker label</option>');
    expect(html.match(/href="\/projects\/project_nav\/jobs\/job_one\?status=completed&amp;worker_id=worker_one"/g)).toHaveLength(2);
    expect(html).not.toContain('untrusted.test');
    expect(html).not.toContain('inbox_other');
  });
  it('returns to the exact validated list context, or the default list for direct entry', () => {
    expect(markup('status=all&worker_id=worker_one&return_url=https://untrusted.test', true)).toContain('href="/projects/project_nav/jobs?status=all&amp;worker_id=worker_one"');
    expect(markup('', true)).toContain('href="/projects/project_nav/jobs"');
    expect(markup('status=completed&status=parked&worker_id=../other', true)).toContain('href="/projects/project_nav/jobs"');
  });
});
