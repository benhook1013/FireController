import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter, createMemoryRouter, RouterProvider } from 'react-router';
import { QueryClient, QueryClientProvider, QueryObserver } from '@tanstack/react-query';
import { cachedWorkerLabelsQuery, ChecklistItems, CheckpointContent, ConversationMessages, Inbox, JobDetail, JobUpdateContent } from './App';
import { NoteContent, NoteRevisionContent, NoteSummaryRows, PauseContent } from './coordination';
import { WorkspaceContext, type Session } from './workspace';
import { BrowserActionMemory } from './browser-actions';
import type { Checkpoint, Job, JobUpdate, Note, Worker } from './types';

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

const messageKey = ['fc', 'connection_one', 'instance_one', 'project_one', 'worker_one'] as const;
const selectedWorker: Worker = { id: 'worker_one', project_id: 'project_one', name: 'Recipient' };
function renderConversation(body: string, configure?: (client: QueryClient) => void, worker = selectedWorker, inbox = false) {
  const client = new QueryClient();
  const key = messageKey;
  client.setQueryData([...key, 'thread', 'conversation_one', ''], { items: [{ id: 'message_one', conversation_id: 'conversation_one', sender_worker_id: 'worker_sender', recipient_worker_id: 'worker_one', body, created_at }], has_more: false });
  client.setQueryData([...key, 'inbox', ''], { items: [{ id: 'message_one', conversation_id: 'conversation_one', sender_worker_id: 'worker_sender', recipient_worker_id: 'worker_one', preview: body, preview_truncated: false, seen: false, acknowledged: false, created_at }], has_more: false });
  configure?.(client);
  const session = { project: { id: 'project_one' }, controller: new AbortController(), api: { browserCapabilities: { allow_writes: false, can_edit_presentation: false } }, worker, actions: new BrowserActionMemory(() => 'unused') } as Session;
  try {
    return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: true }}>{inbox ? <Inbox /> : <ConversationMessages id="conversation_one" />}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
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

describe('message worker labels from the existing scoped worker page', () => {
  it('shows escaped friendly names with exact accessible identities in inbox and conversation headers', () => {
    const configure = (client: QueryClient) => client.setQueryData([...messageKey, 'workers', ''], { items: [{ id: 'worker_sender', project_id: 'project_one', name: 'Builder <one>' }], has_more: false });
    const conversation = renderConversation('Ordinary message.', configure);
    expect(conversation).toContain('<span title="worker_sender">Builder &lt;one&gt;<span class="visually-hidden"> (worker_sender)</span></span>');
    expect(conversation).toContain('<span title="worker_one">Recipient<span class="visually-hidden"> (worker_one)</span></span>');
    const inbox = renderConversation('Ordinary preview.', configure, selectedWorker, true);
    expect(inbox).toContain('aria-label="Read conversation from Builder &lt;one&gt; (worker_sender)"');
    expect(inbox).not.toContain('<one>');
  });
  it('keeps unknown and off-page IDs literal and ignores foreign project, connection and selected-worker names', () => {
    const configure = (client: QueryClient) => {
      client.setQueryData([...messageKey, 'workers', ''], { items: [{ id: 'worker_sender', project_id: 'foreign_project', name: 'Foreign row' }], has_more: true, cursor: 'next' });
      client.setQueryData([...messageKey, 'workers', 'next'], { items: [{ id: 'worker_sender', project_id: 'project_one', name: 'Off-page name' }], has_more: false });
      client.setQueryData(['fc', 'other_connection', 'instance_one', 'project_one', 'worker_one', 'workers', ''], { items: [{ id: 'worker_sender', project_id: 'project_one', name: 'Other connection' }], has_more: false });
      client.setQueryData(['fc', 'connection_one', 'instance_one', 'foreign_project', 'worker_one', 'workers', ''], { items: [{ id: 'worker_sender', project_id: 'foreign_project', name: 'Other project' }], has_more: false });
    };
    const html = renderConversation('Message.', configure, { ...selectedWorker, project_id: 'foreign_project', name: 'Foreign selection' });
    expect(html).toContain('<span title="worker_sender">worker_sender</span>');
    expect(html).toContain('<span title="worker_one">worker_one</span>');
    expect(html).not.toMatch(/Foreign row|Off-page name|Other connection|Other project|Foreign selection/u);
    expect(renderConversation('Preview.', undefined, selectedWorker, true)).toContain('aria-label="Read conversation from worker_sender"');
  });
  it('keeps case-sensitive friendly labels distinguishable by exact tooltip and accessible IDs', () => {
    const html = renderConversation('Message.', client => client.setQueryData([...messageKey, 'workers', ''], { items: [{ id: 'worker_sender', project_id: 'project_one', name: 'Builder' }], has_more: false }), { ...selectedWorker, name: 'builder' });
    expect(html).toContain('<span title="worker_sender">Builder<span class="visually-hidden"> (worker_sender)</span></span>');
    expect(html).toContain('<span title="worker_one">builder<span class="visually-hidden"> (worker_one)</span></span>');
  });
  it('reacts to the sidebar cache arriving later without fetching or draining worker pages', async () => {
    const fetchWorkers = vi.fn().mockResolvedValue({ items: [], has_more: false });
    const client = new QueryClient({ defaultOptions: { queries: { queryFn: fetchWorkers } } });
    const observer = new QueryObserver(client, cachedWorkerLabelsQuery(messageKey));
    const updates: unknown[] = [];
    const unsubscribe = observer.subscribe(value => updates.push(value.data));
    try {
      expect(observer.getCurrentResult().data).toBeUndefined();
      const page = { items: [{ id: 'worker_sender', project_id: 'project_one', name: 'Arrived later' }], has_more: true, cursor: 'next' };
      client.setQueryData([...messageKey, 'workers', ''], page);
      expect(updates.at(-1)).toEqual(page);
      expect(observer.getCurrentResult().data).toEqual(page);
      await client.invalidateQueries({ queryKey: [...messageKey, 'workers'] });
      expect(fetchWorkers).not.toHaveBeenCalled();
      expect(client.getQueryData([...messageKey, 'workers', 'next'])).toBeUndefined();
    } finally { unsubscribe(); observer.destroy(); client.clear(); }
  });
});
