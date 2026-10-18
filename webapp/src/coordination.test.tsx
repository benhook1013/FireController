import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter, createMemoryRouter, RouterProvider } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { WorkspaceContext, type Session } from './workspace';
import { NoteContent, NoteHistoryRows, NoteRevisionContent, NoteSummaryRows, PauseContent, PrimaryContent, WorkerCoordinationView, notesPath } from './coordination';
import type { Note, NoteRevision } from './types';

const home = '/projects/project_one';
const note: Note = { id: 'note_one', project_id: 'project_one', worker_id: 'worker_one', job_id: 'job_one', phase: 'Phase <literal>' + '長'.repeat(40), kind: 'source', status: 'dismissed', status_reason: 'Reason <img src=x>\nSecond line\ttab', body: 'FULL_PRIVATE_BODY <script>instruction()</script>\nExact\ttext', revision: 7, created_at: '2026-10-16T01:00:00.000000000Z', updated_at: '2026-10-16T00:00:00.000000000Z' };
const summary = { ...note, body_preview: 'Preview <img src=x>', body_truncated: true };
const recorded: NoteRevision = { ...note, revision: 2, body: 'CAPTURED_BODY <script>earlier()</script>', status_reason: 'CAPTURED_REASON', actor_id: 'principal_one', actor_worker_id: 'worker_actor', recorded_at: note.updated_at };
function render(value: React.ReactNode) { return renderToStaticMarkup(<MemoryRouter>{value}</MemoryRouter>); }

describe('read-only coordination rendering', () => {
  it('keeps compact previews distinct from complete content, even when not truncated', () => {
    const html = render(<NoteSummaryRows rows={[summary, { ...summary, id: 'note_two', body_truncated: false }]} home={home} />);
    expect(html).toContain('source · dismissed');
    expect(html).toContain('Revision 7');
    expect(html).toContain('Truncated preview.');
    expect(html).toContain('Preview.');
    expect(html.match(/Open the note to read its full body/gu)).toHaveLength(2);
    expect(html).toContain('Preview &lt;img src=x&gt;');
    expect(html).toContain('href="/projects/project_one/notes/note_one"');
    expect(html).not.toMatch(/FULL_PRIVATE_BODY|instruction|Reason|<img|<script|<button/u);
  });
  it('renders exact full note text, dismissal reason and all selectors without interpreting authority', () => {
    const html = render(<NoteContent value={note} home={home} />);
    expect(html).toContain('Full body');
    expect(html).toContain('FULL_PRIVATE_BODY &lt;script&gt;instruction()&lt;/script&gt;\nExact\ttext');
    expect(html).toContain('Dismissal reason');
    expect(html).toContain('Reason &lt;img src=x&gt;\nSecond line\ttab');
    expect(html).toContain('Worker selector');
    expect(html).toContain('Job selector');
    expect(html).toContain('Phase selector');
    expect(html).toContain('Phase &lt;literal&gt;' + '長'.repeat(40));
    expect(html).toContain('href="/projects/project_one/workers/worker_one"');
    expect(html).toContain('href="/projects/project_one/jobs/job_one"');
    expect(html).not.toMatch(/<script|<img|href="javascript:|<button/u);
  });
  it('makes empty selectors explicit without suggesting assignment ownership', () => {
    const html = render(<NoteContent value={{ ...note, worker_id: '', job_id: '', phase: 'phase-only', status: 'pending', status_reason: '' }} home={home} />);
    expect(html).toContain('No worker selector');
    expect(html).toContain('No job selector');
    expect(html).toContain('phase-only');
    expect(html).toContain('No status reason recorded.');
    expect(html).not.toMatch(/href=|owner|dismissed/u);
  });
  it('keeps history rows compact and requires an explicit captured revision read', () => {
    const html = render(<NoteHistoryRows rows={[{ ...summary, actor_id: recorded.actor_id, actor_worker_id: recorded.actor_worker_id, recorded_at: recorded.recorded_at }]} home={home} noteId={note.id} />);
    expect(html).toContain('Read revision 7');
    expect(html).toContain('href="/projects/project_one/notes/note_one/history/7"');
    expect(html).toContain('Recorded by principal_one');
    expect(html).toContain('Worker worker_actor');
    expect(html).toContain('Truncated preview.');
    expect(html).not.toMatch(/FULL_PRIVATE_BODY|Reason|CAPTURED_BODY|CAPTURED_REASON/u);
    const snapshot = render(<NoteRevisionContent value={recorded} home={home} />);
    expect(snapshot).toContain('Captured note revision 2');
    expect(snapshot).toContain('does not describe current state');
    expect(snapshot).toContain('CAPTURED_BODY &lt;script&gt;earlier()&lt;/script&gt;');
    expect(snapshot).toContain('CAPTURED_REASON');
    expect(snapshot).toContain('href="/projects/project_one/notes/note_one"');
    expect(snapshot).toContain('Read current note');
    expect(snapshot).not.toMatch(/FULL_PRIVATE_BODY|<script/u);
  });
  it('shows explicit pause independently from primary focus and never expands a job brief', () => {
    const paused = render(<><PauseContent value={{ worker_id: 'worker_one', revision: 4, paused: true, reason: 'Paused <script>literal</script>\nwhy' }} /><PrimaryContent value={{ worker_id: 'worker_one', revision: 2, job_id: 'job_one' }} home={home} /></>);
    expect(paused).toContain('Pause revision 4');
    expect(paused).toContain('Focus revision 2');
    expect(paused).toContain('Paused &lt;script&gt;literal&lt;/script&gt;\nwhy');
    expect(paused).toContain('not agent presence or an execution control');
    expect(paused).toContain('href="/projects/project_one/jobs/job_one"');
    expect(paused).not.toMatch(/<script|brief|<button/u);
    const defaults = render(<><PauseContent value={{ worker_id: 'worker_one', revision: 0, paused: false, reason: '' }} /><PrimaryContent value={{ worker_id: 'worker_one', revision: 0, job_id: '' }} home={home} /></>);
    expect(defaults).toContain('Not paused');
    expect(defaults).toContain('No pause reason recorded.');
    expect(defaults).toContain('No primary job selected.');
    expect(defaults).toContain('Focus revision 0');
    expect(defaults).not.toContain('href=');
  });
  it('builds explicit worker/job query presets without assignment expansion or cursor inheritance', () => {
    expect(notesPath(home)).toBe(home + '/notes');
    expect(notesPath(home, {worker_id:'worker_one'})).toBe(home + '/notes?worker_id=worker_one');
    expect(notesPath(home, {job_id:'job_one'})).toBe(home + '/notes?job_id=job_one');
    const url = new URL(notesPath(home, {worker_id:'literal&worker', job_id:'literal?job'}), 'https://example.invalid');
    expect([...url.searchParams]).toEqual([['worker_id', 'literal&worker'], ['job_id', 'literal?job']]);
    expect(url.searchParams.has('include_assigned_jobs')).toBe(false);
    expect(url.searchParams.has('cursor')).toBe(false);
  });
});


describe('worker pending-note placement', () => {
  it('shows direct pending notes after the worker identity and before pause/focus details', () => {
    const client = new QueryClient();
    const key = ['fc', 'connection_one', 'instance_one', 'project_one', ''] as const;
    const workerId = 'worker_one';
    client.setQueryData([...key, 'worker', workerId], { id: workerId, project_id: 'project_one', name: 'Worker display name' });
    client.setQueryData([...key, 'worker-pause', workerId], { worker_id: workerId, paused: false, revision: 0, reason: '' });
    client.setQueryData([...key, 'worker-primary', workerId], { worker_id: workerId, job_id: '', revision: 0 });
    client.setQueryData([...key, 'notes', { worker_id: workerId, include_assigned_jobs: false, status: 'pending', limit: 5 }, ''], { items: [{ ...summary, status: 'pending' }], has_more: false });
    const session = { controller: new AbortController(), api: {} } as Session;
    const router = createMemoryRouter([{ path: '/projects/project_one/workers/:workerId', element: <WorkerCoordinationView /> }], { initialEntries: [home + '/workers/' + workerId] });
    try {
      const html = renderToStaticMarkup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: false }}><RouterProvider router={router} /></WorkspaceContext.Provider></QueryClientProvider>);
      expect(html.indexOf('Worker display name')).toBeLessThan(html.indexOf('Pending notes'));
      expect(html.indexOf('Pending notes')).toBeLessThan(html.indexOf('Explicit pause'));
      expect(html).toContain('Preview &lt;img src=x&gt;');
      expect(html).toContain('Primary job');
      expect(html).not.toContain('Notes for this worker');
      expect(html).toContain('View all notes and history');
    } finally { router.dispose(); client.clear(); }
  });
});
