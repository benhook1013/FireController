import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ConversationMessages, Inbox } from './App';
import { BrowserActionMemory } from './browser-actions';
import { WorkspaceContext, type Session } from './workspace';
import type { InboxItem, Message } from './types';

const at = '2026-10-15T12:00:00Z';
const controller: Message = { origin_kind: 'controller', id: 'message_one', conversation_id: 'conversation_one', sender_worker_id: '', recipient_worker_id: 'worker_one', body: '**Review ready**\n\n[Read evidence](https://example.com/proof)\n\n<script>unsafe()</script>\n\n![Remote](https://example.com/image.png)', created_at: at };
function render(message: Message, inbox = false, writes = true, flags = { seen: false, acknowledged: false }) {
  const client = new QueryClient(), key = ['fc', 'session', 'instance_one', 'project_one', 'worker_one'];
  const item: InboxItem = { ...message, preview: '**Review ready** <script>literal</script>', preview_truncated: false, ...flags };
  client.setQueryData([...key, 'thread', message.conversation_id, ''], { conversation_id: message.conversation_id, root_message_id: message.id, items: [message], has_more: false });
  client.setQueryData([...key, 'inbox', ''], { items: [item], has_more: false });
  client.setQueryData([...key, 'workers', ''], { items: [{ id: 'worker_sender', project_id: 'project_one', name: 'Builder' }], has_more: false });
  const api = { browserCapabilities: { allow_writes: writes, can_edit_presentation: false }, sendMessage: vi.fn(), markMessageSeen: vi.fn(), acknowledgeMessage: vi.fn() };
  const session = { project: { id: 'project_one' }, worker: { id: 'worker_one', project_id: 'project_one', name: 'Recipient' }, controller: new AbortController(), api, actions: new BrowserActionMemory() } as unknown as Session;
  try {
    const html = renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project_one', selectWorker: vi.fn(), fail: vi.fn(), ready: true }}>{inbox ? <Inbox /> : <ConversationMessages id={message.conversation_id} />}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
    return { html, api };
  } finally { client.clear(); }
}
describe('Controller notifications in existing message views', () => {
  it('labels the inbox author Controller and retains ordinary seen/ack controls without a fake worker identity', () => {
    const { html, api } = render(controller, true);
    expect(html).toContain('<strong>Controller</strong>');
    expect(html).toContain('aria-label="Read conversation from Controller"');
    expect(html).toContain('Mark seen'); expect(html).toContain('Acknowledge');
    expect(html).toContain('**Review ready** &lt;script&gt;literal&lt;/script&gt;');
    expect(html).not.toContain('title=""'); expect(html).not.toContain('>Reply</button>');
    expect(api.sendMessage).not.toHaveBeenCalled(); expect(api.markMessageSeen).not.toHaveBeenCalled(); expect(api.acknowledgeMessage).not.toHaveBeenCalled();
    const settled = render(controller, true, true, { seen: true, acknowledged: true }).html;
    expect(settled).toContain('Acknowledged'); expect(settled).not.toContain('Mark seen'); expect(settled).not.toContain('>Acknowledge</button>');
  });
  it('renders a safe Markdown Controller conversation and recipient name with no reply composer for a writer', () => {
    const { html, api } = render(controller);
    expect(html).toContain('<strong>Controller<span class="message-direction">');
    expect(html).toContain('Recipient'); expect(html).toContain('<strong>Review ready</strong>');
    expect(html).toContain('href="https://example.com/proof"');
    expect(html).toContain('&lt;script&gt;'); expect(html).not.toMatch(/<script|<img|<textarea|>Reply</u);
    expect(api.sendMessage).not.toHaveBeenCalled();
  });
  it('preserves worker author labels and reply controls, and respects read-only sessions', () => {
    const worker = { ...controller, sender_worker_id: 'worker_sender' }; delete worker.origin_kind;
    const ordinary = render(worker).html;
    expect(ordinary).toContain('title="worker_sender">Builder'); expect(ordinary).toContain('>Reply</button>');
    expect(render(worker, false, false).html).not.toContain('>Reply</button>');
    const readOnly = render(controller, true, false).html;
    expect(readOnly).toContain('Read conversation from Controller'); expect(readOnly).not.toContain('Mark seen'); expect(readOnly).not.toContain('>Acknowledge</button>');
  });
});
