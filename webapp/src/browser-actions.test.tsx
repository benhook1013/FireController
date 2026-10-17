import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ApiError } from './api';
import { BrowserActionMemory, MessageComposer, RecipientActions, actionEndsAccess } from './browser-actions';
import type { InboxItem } from './types';

const context = vi.hoisted(() => ({ value: {} as Record<string, unknown> }));
vi.mock('./App', () => ({ useWorkspace: () => context.value, signalFor: (session: { controller: AbortController }, signal: AbortSignal) => AbortSignal.any([session.controller.signal, signal]) }));
function workspace(allowWrites = true, worker = 'worker_one', actions = new BrowserActionMemory(() => 'test_key')) {
  const api = { browserCapabilities: { allow_writes: allowWrites, can_edit_presentation: false }, sendMessage: vi.fn(), markMessageSeen: vi.fn(), acknowledgeMessage: vi.fn() };
  context.value = { session: { api, actions, controller: new AbortController(), worker: worker ? { id: worker, name: 'Worker' } : null }, key: ['test', worker], home: '/projects/project_one', fail: vi.fn() };
  return api;
}
function render(value: React.ReactNode) { return renderToStaticMarkup(<QueryClientProvider client={new QueryClient()}>{value}</QueryClientProvider>); }
const item: InboxItem = { id: 'message_one', conversation_id: 'conversation_one', sender_worker_id: 'worker_sender', recipient_worker_id: 'worker_one', preview: 'Read only', preview_truncated: false, seen: false, acknowledged: false, created_at: '2026-10-17T01:00:00Z' };

describe('explicit browser actions', () => {
  it('deduplicates simultaneous submission and retains exact worker/body/key across unknown retry', async () => {
    let count = 0;
    const actions = new BrowserActionMemory(() => `key_${++count}`);
    const input = { worker: 'worker_one', recipient: 'worker_two', body: 'Original <literal>', replyTo: 'message_one', config: { name: 'Frozen' } };
    let reject!: (error: unknown) => void;
    const execute = vi.fn(() => new Promise<unknown>((_, fail) => { reject = fail; }));
    const signal = new AbortController().signal;
    const first = actions.run('worker_one/reply', input, execute, signal);
    const second = actions.run('worker_one/reply', { ...input, body: 'Changed double click' }, execute, signal);
    expect(first).toBe(second);
    input.body = 'Later draft'; input.config.name = 'Later setting';
    await Promise.resolve();
    reject(new ApiError('connection_failed', 'PRIVATE_TRANSPORT_DETAIL', 0, true));
    await expect(first).rejects.toMatchObject({ outcomeUnknown: true });
    expect(execute).toHaveBeenCalledTimes(1);
    expect(actions.get('worker_one/reply')).toMatchObject({ key: 'key_1', input: { worker: 'worker_one', body: 'Original <literal>', config: { name: 'Frozen' } }, uncertain: true });
    const retry = vi.fn(async (value: typeof input, key: string) => ({ value, key }));
    const result = await actions.run('worker_one/reply', { ...input, worker: 'worker_wrong', body: 'Must not replace' }, retry, signal);
    expect(result).toMatchObject({ key: 'key_1', value: { worker: 'worker_one', recipient: 'worker_two', body: 'Original <literal>', replyTo: 'message_one' } });
    expect(count).toBe(1); expect(actions.get('worker_one/reply')).toBeUndefined();
  });
  it('notifies a remounted view when a deferred pending action becomes uncertain without changing the unresolved count', async () => {
    const actions = new BrowserActionMemory(() => 'original_key');
    const slot = JSON.stringify(['worker_one', 'send', 'new']);
    const input = { worker: 'worker_one', recipient: 'worker_two', body: 'Original pending message', replyTo: '' };
    let reject!: (error: unknown) => void;
    const firstView = vi.fn();
    const unmount = actions.subscribe(firstView);
    const pending = actions.run(slot, input, () => new Promise((_, fail) => { reject = fail; }), new AbortController().signal);
    await Promise.resolve();
    expect(firstView).toHaveBeenCalledTimes(1);
    expect(actions.get(slot)?.running).toBe(true);
    const mountedRevision = actions.revision;
    unmount();
    const remountedView = vi.fn();
    const unsubscribe = actions.subscribe(remountedView);
    reject(new ApiError('connection_failed', 'Unknown after view change', 0, true));
    await expect(pending).rejects.toMatchObject({ outcomeUnknown: true });
    expect(actions.unresolved).toBe(1);
    expect(actions.revision).toBeGreaterThan(mountedRevision);
    expect(firstView).toHaveBeenCalledTimes(1);
    expect(remountedView).toHaveBeenCalledTimes(1);
    expect(actions.get(slot)).toMatchObject({ running: false, uncertain: true, key: 'original_key', input });
    workspace(true, 'worker_one', actions);
    const html = render(<MessageComposer />);
    expect(html).toContain('Retry exact send');
    expect(html).not.toContain('Sending…');
    expect(html).toContain('Original pending message');
    const retry = vi.fn(async (value: typeof input, key: string) => ({ value, key }));
    await expect(actions.run(slot, { ...input, body: 'Replacement draft' }, retry, new AbortController().signal)).resolves.toEqual({ value: input, key: 'original_key' });
    expect(remountedView).toHaveBeenCalledTimes(3);
    expect(actions.unresolved).toBe(0);
    unsubscribe();
  });
  it('isolates worker slots, retains canceled possible commits and never invokes work for an already closed view', async () => {
    const actions = new BrowserActionMemory(() => 'retained_key');
    const controller = new AbortController();
    const failure = new ApiError('cancelled', 'Detached', 0, true);
    await expect(actions.run('worker_one/send', { worker: 'worker_one', body: 'Private' }, async () => { controller.abort(); throw failure; }, controller.signal)).rejects.toBe(failure);
    expect(actions.get('worker_one/send')).toMatchObject({ uncertain: true });
    expect(actions.get('worker_two/send')).toBeUndefined();
    const never = vi.fn(async () => null);
    await expect(actions.run('worker_two/send', {}, never, controller.signal)).rejects.toMatchObject({ code: 'cancelled' });
    expect(never).not.toHaveBeenCalled(); expect(actions.get('worker_one/send')).toBeDefined();
    const differentProject = new BrowserActionMemory(); expect(differentProject.get('worker_one/send')).toBeUndefined();
  });
  it('bounds unresolved memory and releases known refusals without treating action403 as logout', async () => {
    const actions = new BrowserActionMemory(() => 'key');
    const signal = new AbortController().signal;
    await expect(actions.run('denied', {}, async () => { throw new ApiError('forbidden', 'Recipient denied', 403); }, signal)).rejects.toMatchObject({ status: 403 });
    expect(actions.get('denied')).toBeUndefined();
    expect(actionEndsAccess(new ApiError('forbidden', 'No action permission', 403))).toBe(false);
    expect(actionEndsAccess(new ApiError('unauthorized', 'Revoked', 401))).toBe(true);
    expect(actionEndsAccess(new ApiError('instance_mismatch', 'Changed', 409))).toBe(true);
    for (let i = 0; i < 32; i++) await actions.run(`slot_${i}`, {}, async () => { throw new ApiError('connection_failed', 'Uncertain', 0, true); }, signal).catch(() => {});
    const never = vi.fn(async () => null);
    await expect(actions.run('overflow', {}, never, signal)).rejects.toMatchObject({ code: 'action_capacity' });
    expect(never).not.toHaveBeenCalled();
  });
  it('renders no mutation controls or calls for read-only or unselected workers', () => {
    const api = workspace(false); expect(render(<><MessageComposer /><RecipientActions item={item} /></>)).toBe(''); expect(api.sendMessage).not.toHaveBeenCalled(); expect(api.markMessageSeen).not.toHaveBeenCalled();
    workspace(true, ''); expect(render(<><MessageComposer /><RecipientActions item={item} /></>)).toBe('');
    workspace(true, 'worker_other'); expect(render(<RecipientActions item={item} />)).toBe('');
    workspace(); const html = render(<RecipientActions item={item} />); expect(html).toContain('Mark seen'); expect(html).toContain('Acknowledge');
  });
  it('restores locked retry content in its original worker view, including seen state updated by live reads', async () => {
    const actions = new BrowserActionMemory(() => 'original_key');
    const input = { worker: 'worker_one', recipient: 'worker_two', body: 'Exact <script>literal</script>', replyTo: '' };
    await actions.run(JSON.stringify(['worker_one', 'send', 'new']), input, async () => { throw new ApiError('connection_failed', 'PRIVATE_ERROR', 0, true); }, new AbortController().signal).catch(() => {});
    workspace(true, 'worker_one', actions);
    const html = render(<MessageComposer />);
    expect(html).toContain('Retry exact send'); expect(html).toContain('Exact &lt;script&gt;literal&lt;/script&gt;'); expect(html).toContain('disabled=""'); expect(html).not.toContain('PRIVATE_ERROR'); expect(html).not.toContain('<script>');
    await actions.run(JSON.stringify(['worker_one', 'seen', item.id]), { worker: 'worker_one', id: item.id }, async () => { throw new ApiError('connection_failed', 'Unknown', 0, true); }, new AbortController().signal).catch(() => {});
    expect(render(<RecipientActions item={{ ...item, seen: true }} />)).toContain('Retry exact seen');
  });
});
