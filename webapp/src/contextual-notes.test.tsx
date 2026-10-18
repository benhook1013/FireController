import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider, QueryObserver } from '@tanstack/react-query';
import { ContextualNoteRows, ContextualNotes, contextualNoteScope, contextualNotesQuery } from './contextual-notes';
import { WorkspaceContext, type Session } from './workspace';
import type { NoteOptions, NoteSummary, Page } from './types';

const note: NoteSummary = { id: 'note_one', project_id: 'project_one', revision: 1, worker_id: 'worker_one', job_id: '', phase: '', kind: 'reminder', status: 'pending', created_at: '2026-10-18T00:00:00Z', updated_at: '2026-10-18T00:00:00Z', body_preview: 'Remember <script>literal</script>', body_truncated: false };
const empty = { items: [], has_more: false } satisfies Page<NoteSummary>;
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done; }); return { promise, resolve }; }
function environment(project = 'project_one', read = vi.fn()) {
  const key = ['fc', `connection_${project}`, 'instance_one', project, 'inbox_actor'] as const;
  const session = { controller: new AbortController(), api: { notes: read } } as unknown as Session;
  return { key, session, read };
}
function renderPanel(value?: Page<NoteSummary>, error?: Error, target: { workerId: string } | { jobId: string } = { workerId: 'worker_one' }) {
  const env = environment();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, retryOnMount: false } } });
  const scope = contextualNoteScope(target)!;
  const options = contextualNotesQuery(env.session, env.key, scope);
  if (value) client.setQueryData(options.queryKey, value);
  if (error) client.getQueryCache().build(client, options).setState({ status: 'error', error });
  try {
    return renderToStaticMarkup(<QueryClientProvider client={client}><MemoryRouter><WorkspaceContext.Provider value={{ ...env, home: '/projects/project_one', selectWorker: () => {}, fail: () => {}, ready: false }}><ContextualNotes {...target} /></WorkspaceContext.Provider></MemoryRouter></QueryClientProvider>);
  } finally { client.clear(); }
}

describe('contextual pending notes', () => {
  it('requires exactly one valid target and never expands worker assignments', () => {
    expect(contextualNoteScope({ workerId: 'worker_one' })).toEqual({ worker_id: 'worker_one', include_assigned_jobs: false });
    expect(contextualNoteScope({ jobId: 'job_one' })).toEqual({ job_id: 'job_one' });
    for (const target of [{}, { workerId: '' }, { jobId: 'not/a/job' }, { workerId: 'worker_one', jobId: 'job_one' }]) expect(contextualNoteScope(target as never)).toBeUndefined();
  });
  it('hides fetched-empty state but keeps concise loading and safe failure visible', () => {
    expect(renderPanel(empty)).toBe('');
    const loading = renderPanel();
    expect(loading).toContain('Pending notes');
    expect(loading).toContain('aria-label="Loading pending notes"');
    expect(loading).not.toContain('Loading…');
    const failed = renderPanel(undefined, new Error('PRIVATE_RAW_ERROR'));
    expect(failed).toContain('Pending notes');
    expect(failed).toContain('role="alert"');
    expect(failed).not.toContain('PRIVATE_RAW_ERROR');
    expect(failed).toContain('View all notes and history');
  });
  it('shows bounded pending previews with clear detail/history links and no repeated scope IDs', () => {
    const html = renderPanel({ items: [note], has_more: true, cursor: 'opaque_cursor' });
    expect(html).toContain('Remember &lt;script&gt;literal&lt;/script&gt;');
    expect(html).toContain('href="/projects/project_one/notes/note_one"');
    expect(html).toContain('Read note');
    expect(html).toContain('status=all&amp;worker_id=worker_one');
    expect(html).toContain('Next pending notes');
    expect(html).not.toMatch(/Worker selector|Job selector|Revision|include_assigned_jobs|<script/u);
    expect(html.replace(/<[^>]*>/gu, '')).not.toContain('worker_one');
    const job = renderPanel({ items: [{ ...note, worker_id: '', job_id: 'job_one' }], has_more: false }, undefined, { jobId: 'job_one' });
    expect(job).toContain('status=all&amp;job_id=job_one');
    expect(job).not.toContain('worker_id=');
  });
  it('renders previews as text without treating them as instructions or complete note bodies', () => {
    const html = renderToStaticMarkup(<MemoryRouter><ContextualNoteRows notes={[note]} home="/projects/project_one" /></MemoryRouter>);
    expect(html).not.toContain('<script');
    expect(html).not.toContain('consumed');
    expect(html).not.toContain('<button');
  });
  it('sends one finite pending read with an explicit direct target and cancellation', async () => {
    const env = environment();
    env.read.mockResolvedValue(empty);
    const query = contextualNotesQuery(env.session, env.key, contextualNoteScope({ workerId: 'worker_one' })!, 'cursor_one');
    const request = new AbortController();
    await query.queryFn({ signal: request.signal });
    expect(env.read).toHaveBeenCalledTimes(1);
    const [options, signal] = env.read.mock.calls[0];
    expect(options).toEqual({ worker_id: 'worker_one', include_assigned_jobs: false, status: 'pending', limit: 5, cursor: 'cursor_one' });
    expect(query.queryKey.slice(0, env.key.length)).toEqual(env.key);
    expect(query.queryKey[env.key.length]).toBe('notes');
    request.abort(); expect(signal.aborted).toBe(true);
  });
  it('cancels old target/project reads and never surfaces their late data in the new context', async () => {
    for (const switchProject of [false, true]) {
      const oldRead = deferred<Page<NoteSummary>>(), newRead = deferred<Page<NoteSummary>>();
      const observed: { options: NoteOptions; signal: AbortSignal }[] = [];
      const oldEnv = environment('project_one', vi.fn((options, signal) => { observed.push({ options, signal }); return oldRead.promise; }));
      const nextEnv = switchProject ? environment('project_two', vi.fn(() => newRead.promise)) : oldEnv;
      if (!switchProject) oldEnv.read.mockImplementationOnce((options, signal) => { observed.push({ options, signal }); return oldRead.promise; }).mockImplementation(() => newRead.promise);
      const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      const oldOptions = contextualNotesQuery(oldEnv.session, oldEnv.key, contextualNoteScope({ workerId: 'worker_one' })!);
      const nextOptions = contextualNotesQuery(nextEnv.session, nextEnv.key, contextualNoteScope({ workerId: 'worker_two' })!);
      const observer = new QueryObserver(client, oldOptions);
      const stop = observer.subscribe(() => {});
      try {
        await vi.waitFor(() => expect(observed).toHaveLength(1));
        observer.setOptions(nextOptions);
        expect(observed[0].signal.aborted).toBe(true);
        oldRead.resolve({ items: [note], has_more: false });
        const current = { ...note, id: 'note_two', worker_id: 'worker_two', project_id: switchProject ? 'project_two' : 'project_one' };
        newRead.resolve({ items: [current], has_more: false });
        await vi.waitFor(() => expect(observer.getCurrentResult().data?.items).toEqual([current]));
        expect(client.getQueryData(oldOptions.queryKey)).toBeUndefined();
      } finally { stop(); client.clear(); }
    }
  });
});
