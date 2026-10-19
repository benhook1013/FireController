import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter, createMemoryRouter, RouterProvider } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ChecklistItems, CheckpointContent, ConversationMessages, JobDetail, JobUpdateContent } from './App';
import { NoteContent, NoteRevisionContent, NoteSummaryRows, PauseContent } from './coordination';
import { WorkspaceContext, type Session } from './workspace';
import { BrowserActionMemory } from './browser-actions';
import type { Checkpoint, Job, JobUpdate, Note } from './types';

const home = '/projects/project_one';
const created_at = '2026-10-19T00:00:00Z';
const checkpoint: Checkpoint = { id: 'checkpoint_one', project_id: 'project_one', job_id: 'job_one', job_revision: 3, sequence: 2, actor_id: 'actor_one', worker_id: 'worker_one', done: '', next: '', blocker: '', evidence: ['**literal evidence reference**'], created_at };
const update: JobUpdate = { id: 'update_one', project_id: 'project_one', job_id: 'job_one', job_revision: 3, sequence: 1, actor_id: 'actor_one', worker_id: 'worker_one', body: '', evidence: ['**literal evidence reference**'], created_at };
const note: Note = { id: 'note_one', project_id: 'project_one', worker_id: 'worker_one', job_id: 'job_one', phase: '', kind: 'reminder', status: 'pending', status_reason: '', body: '', revision: 2, created_at, updated_at: created_at };
const render = (node: React.ReactNode) => renderToStaticMarkup(<MemoryRouter>{node}</MemoryRouter>);

function renderJob(brief: string) {
  const client = new QueryClient();
  const key = ['fc', 'connection_one', 'instance_one', 'project_one', ''] as const;
  const job: Job = { id: 'job_one', project_id: 'project_one', title: 'Current job', worker_id: '', status: 'active', revision: 3, brief, created_at, updated_at: created_at };
  client.setQueryData([...key, 'job', job.id], job);
  client.setQueryData([...key, 'checklist-current', job.id], { job_id: job.id, job_revision: job.revision, revision: 1, items: [] });
  client.setQueryData([...key, 'checkpoint-latest', job.id], { job_id: job.id, job_revision: job.revision, checkpoint: null });
  client.setQueryData([...key, 'notes', { job_id: job.id, status: 'pending', limit: 5 }, ''], { items: [], has_more: false });
  const session = { controller: new AbortController(), api: {} } as Session;
  const router = createMemoryRouter([{ path: '/projects/project_one/jobs/:jobId', element: <JobDetail /> }], { initialEntries: [home + '/jobs/' + job.id] });
  try {
    return renderToStaticMarkup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: false }}><RouterProvider router={router} /></WorkspaceContext.Provider></QueryClientProvider>);
  } finally { router.dispose(); client.clear(); }
}

function renderConversation(body: string) {
  const client = new QueryClient();
  const key = ['fc', 'connection_one', 'instance_one', 'project_one', 'worker_one'] as const;
  client.setQueryData([...key, 'thread', 'conversation_one', ''], { items: [{ id: 'message_one', conversation_id: 'conversation_one', sender_worker_id: 'worker_sender', recipient_worker_id: 'worker_one', body, created_at }], has_more: false });
  const session = { controller: new AbortController(), api: { browserCapabilities: { allow_writes: false, can_edit_presentation: false } }, worker: { id: 'worker_one' }, actions: new BrowserActionMemory(() => 'unused') } as Session;
  try {
    return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: false }}><ConversationMessages id="conversation_one" /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
  } finally { client.clear(); }
}

const surfaces = [
  { name: 'full job brief', render: renderJob },
  { name: 'checklist item', render: (text: string) => render(<ChecklistItems items={[{ id: 'item_one', done: false, text }]} />) },
  { name: 'checkpoint narratives', render: (body: string) => render(<CheckpointContent checkpoint={{ ...checkpoint, done: body, next: body, blocker: body }} />) },
  { name: 'narrative update', render: (body: string) => render(<JobUpdateContent update={{ ...update, body }} jobRevision={3} />) },
  { name: 'full current note', render: (body: string) => render(<NoteContent value={{ ...note, body }} home={home} />) },
  { name: 'note status reason', render: (status_reason: string) => render(<NoteContent value={{ ...note, status_reason }} home={home} />) },
  { name: 'worker pause reason', render: (reason: string) => render(<PauseContent value={{ worker_id: 'worker_one', paused: true, revision: 2, reason }} />) },
  { name: 'full conversation message', render: renderConversation },
  { name: 'full captured note', render: (body: string) => render(<NoteRevisionContent value={{ ...note, body, actor_id: 'actor_one', actor_worker_id: 'worker_one', recorded_at: created_at }} home={home} />) },
];

describe('operational authored Markdown', () => {
  for (const surface of surfaces) {
    it(`renders safe structure in ${surface.name} without external image or HTML elements`, () => {
      const html = surface.render(`# Progress

A **finished** step with \`inline code\`.

- First item
- [Evidence](https://example.com/proof)
- [Unsafe](javascript:alert%281%29)
- ![Remote diagram](https://example.com/private.png)

\`\`\`sh
fc inbox list
\`\`\`

<script>window.compromised = true</script>`);
      expect(html).toContain('<h4>Progress</h4>');
      expect(html).toContain('<strong>finished</strong>');
      expect(html).toContain('<code>inline code</code>');
      expect(html).toContain('class="editorial-list editorial-list-unordered"');
      expect(html).toContain('href="https://example.com/proof" target="_blank" rel="noreferrer noopener"');
      expect(html).toContain('class="editorial-code-block"');
      expect(html).toContain('fc inbox list');
      expect(html).toContain('Image: Remote diagram');
      expect(html).toContain('&lt;script&gt;');
      expect(html).not.toMatch(/<script|<img|href="javascript:|src="https:\/\/example.com\/private/u);
    });
    it(`keeps ordinary prose readable in ${surface.name}`, () => {
      const html = surface.render('Ordinary text with a second sentence.');
      expect(html).toContain('<p>Ordinary text with a second sentence.</p>');
    });
  }

  it('keeps compact previews and structured evidence references literal', () => {
    const preview = render(<NoteSummaryRows rows={[{ ...note, body_preview: '**preview only**', body_truncated: true }]} home={home} />);
    expect(preview).toContain('**preview only**');
    expect(preview).not.toContain('<strong>preview only</strong>');
    expect(preview).toContain('Open the note to read its full body.');
    for (const html of [render(<CheckpointContent checkpoint={checkpoint} />), render(<JobUpdateContent update={update} jobRevision={3} />)]) {
      expect(html).toContain('**literal evidence reference**');
      expect(html).not.toContain('<strong>literal evidence reference</strong>');
    }
  });
});
