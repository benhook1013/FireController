import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router';
import { ApiError } from './api';
import { newSessionID } from './browser-session';
import { signalFor, useWorkspace } from './App';
import type { InboxItem, Message } from './types';
import './browser-actions.css';

export type RetainedAction<T> = { key: string; input: T; running: boolean; uncertain: boolean; error?: unknown; promise?: Promise<unknown> };

// A connection's memory holds at most 32 unresolved actions. It survives local
// worker/route remounts, never enters browser storage and never retries itself.
export class BrowserActionMemory {
  private readonly actions = new Map<string, RetainedAction<unknown>>();
  private readonly listeners = new Set<() => void>();
  private currentRevision = 0;
  constructor(private readonly newKey = () => `browser_action_${newSessionID()}`) {}
  subscribe(listener: () => void): () => void { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; }
  get unresolved(): number { return this.actions.size; }
  get revision(): number { return this.currentRevision; }
  private changed(): void { this.currentRevision++; for (const listener of this.listeners) listener(); }
  get<T>(slot: string): RetainedAction<T> | undefined { return this.actions.get(slot) as RetainedAction<T> | undefined; }
  run<T, R>(slot: string, input: T, execute: (input: T, key: string, signal: AbortSignal) => Promise<R>, signal: AbortSignal): Promise<R> {
    let action = this.get<T>(slot);
    if (action?.running) return action.promise as Promise<R>;
    if (signal.aborted) return Promise.reject(new ApiError('cancelled', 'The action view was closed.'));
    if (!action) {
      if (this.actions.size >= 32) return Promise.reject(new ApiError('action_capacity', 'Retry an unresolved action before starting another.'));
      action = { key: this.newKey(), input: structuredClone(input), running: false, uncertain: false };
      this.actions.set(slot, action);
    }
    const retained = action;
    retained.running = true; retained.error = undefined;
    this.changed();
    retained.promise = Promise.resolve().then(() => execute(retained.input, retained.key, signal)).then(value => {
      retained.running = false; this.actions.delete(slot); this.changed(); return value;
    }, error => {
      retained.running = false; retained.error = error;
      retained.uncertain = !(error instanceof ApiError) || error.outcomeUnknown;
      if (!retained.uncertain) this.actions.delete(slot);
      this.changed();
      throw error;
    });
    return retained.promise as Promise<R>;
  }
}

export function actionEndsAccess(error: unknown): boolean {
  return error instanceof ApiError && (error.status === 401 || ['unauthorized', 'instance_mismatch', 'browser_session_expired', 'credential_revoked'].includes(error.code));
}
export function actionErrorText(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.outcomeUnknown) return 'The result is unknown. Retry this exact action using the retained key and content.';
    if (error.status === 403) return 'This action is not permitted for this connection or selected worker. Your readable connection remains open.';
    if (error.status === 409) return 'The record changed. Reload the current settings and reconcile your edit before saving again.';
    if (error.code === 'invalid_request') return 'Check the selected worker and entered values before trying again.';
    if (error.code === 'action_capacity') return 'Retry an unresolved action before starting another.';
    if (actionEndsAccess(error)) return 'Access ended. Connect again to verify this project.';
  }
  return 'The action could not be confirmed. Retry the retained action before starting another.';
}

export function useBrowserAction<T, R>(slot: string, execute: (input: T, key: string, signal: AbortSignal) => Promise<R>, success: (result: R) => void | Promise<void>) {
  const { session, fail } = useWorkspace();
  const controller = useRef(new AbortController());
  const active = useRef(true);
  useSyncExternalStore(useCallback(listener => session.actions.subscribe(listener), [session.actions]), () => session.actions.revision, () => session.actions.revision);
  const [error, setError] = useState<unknown>();
  const [done, setDone] = useState(false);
  useEffect(() => { active.current = true; controller.current = new AbortController(); return () => { active.current = false; controller.current.abort(); }; }, []);
  const retained = session.actions.get<T>(slot);
  const run = async (input: T) => {
    if (session.actions.get<T>(slot)?.running) return;
    setDone(false); setError(undefined);
    const pending = session.actions.run(slot, input, execute, signalFor(session, controller.current.signal));
    try {
      const result = await pending;
      if (!active.current || controller.current.signal.aborted || session.controller.signal.aborted) return;
      setDone(true);
      await success(result);
    } catch (error) {
      if (!active.current || controller.current.signal.aborted || session.controller.signal.aborted) return;
      setError(error);
      if (actionEndsAccess(error)) fail(error);
    }
  };
  return { run, retained, error: error ?? retained?.error, done, busy: retained?.running ?? false, locked: Boolean(retained), clearNotice: () => { setError(undefined); setDone(false); } };
}

export function ActionNotice({ error, done, children }: { error: unknown; done?: boolean; children?: ReactNode }) {
  if (error) return <p className="notice" role="alert">{actionErrorText(error)}</p>;
  return done ? <p className="action-success" role="status">{children ?? 'Action recorded.'}</p> : null;
}

type SendInput = { worker: string; recipient: string; body: string; replyTo: string };
export function MessageComposer({ reply }: { reply?: Message }) {
  const { session, key, home } = useWorkspace();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [recipient, setRecipient] = useState('');
  const [body, setBody] = useState('');
  const [sentConversation, setSentConversation] = useState('');
  const worker = session.worker?.id ?? '';
  const replyRecipient = reply ? reply.sender_worker_id === worker ? reply.recipient_worker_id : reply.sender_worker_id : '';
  const slot = JSON.stringify([worker, 'send', reply?.id ?? 'new']);
  const action = useBrowserAction<SendInput, { id: string; conversation_id: string; outcome: 'sent' }>(slot,
    (input, actionKey, signal) => session.api.sendMessage(input.worker, input.recipient, input.body, input.replyTo, actionKey, signal),
    async receipt => { setBody(''); setSentConversation(receipt.conversation_id); await queryClient.invalidateQueries({ queryKey: [...key, 'inbox'] }); if (reply) await queryClient.invalidateQueries({ queryKey: [...key, 'thread', reply.conversation_id] }); });
  if (!worker || !session.api.browserCapabilities.allow_writes || reply && reply.sender_worker_id !== worker && reply.recipient_worker_id !== worker) return null;
  const input = action.retained?.input;
  const submit = (event: FormEvent) => { event.preventDefault(); void action.run({ worker, recipient: replyRecipient || recipient, body, replyTo: reply?.id ?? '' }); };
  return <section className="browser-composer" aria-label={reply ? 'Reply composer' : 'New message composer'}>
    <button type="button" aria-expanded={open || action.locked} onClick={() => setOpen(value => !value)}>{reply ? 'Reply' : 'New message'}</button>
    {(open || action.locked) && <form className="browser-action-form" onSubmit={submit}>
      <p className="field-note">Send as {worker}.{reply && <> Reply stays in this conversation.</>}</p>
      {!reply && <label>Recipient worker ID<input value={input?.recipient ?? recipient} onChange={event => setRecipient(event.target.value)} disabled={action.locked} required maxLength={128} spellCheck={false} /></label>}
      {reply && <p>To {input?.recipient ?? replyRecipient}</p>}
      <label>{reply ? 'Reply message' : 'Message'}<textarea value={input?.body ?? body} onChange={event => setBody(event.target.value)} disabled={action.locked} required maxLength={16384} rows={4} /></label>
      <button className="primary" type="submit" disabled={action.busy}>{action.busy ? 'Sending…' : action.locked ? 'Retry exact send' : reply ? 'Send reply' : 'Send message'}</button>
      <ActionNotice error={action.error} done={action.done}>Message sent.</ActionNotice>
      {!reply && action.done && sentConversation && <Link to={`${home}/inbox/${encodeURIComponent(sentConversation)}`}>Open sent conversation</Link>}
      {action.retained?.uncertain && <p className="field-note">Recovery key: <code>{action.retained.key}</code>. Keep the displayed recipient and message with this key if you need manual recovery.</p>}
      {action.retained?.uncertain && <p className="field-note">Keep this connection open to retain the original key and content for retry. Switching workers does not move this action.</p>}
    </form>}
  </section>;
}

export function RecipientActions({ item }: { item: InboxItem }) {
  const { session, key } = useWorkspace();
  const queryClient = useQueryClient();
  const worker = session.worker?.id ?? '';
  const refresh = async () => { await queryClient.invalidateQueries({ queryKey: [...key, 'inbox'] }); };
  const seen = useBrowserAction<{ worker: string; id: string }, unknown>(JSON.stringify([worker, 'seen', item.id]), (input, actionKey, signal) => session.api.markMessageSeen(input.worker, input.id, actionKey, signal), refresh);
  const ack = useBrowserAction<{ worker: string; id: string }, unknown>(JSON.stringify([worker, 'ack', item.id]), (input, actionKey, signal) => session.api.acknowledgeMessage(input.worker, input.id, actionKey, signal), refresh);
  if (!worker || !session.api.browserCapabilities.allow_writes || item.recipient_worker_id !== worker) return null;
  return <div className="recipient-actions">
    {(!item.seen || seen.locked) && <button type="button" disabled={seen.busy || seen.done} onClick={() => void seen.run({ worker, id: item.id })}>{seen.busy ? 'Recording…' : seen.locked ? 'Retry exact seen' : 'Mark seen'}</button>}
    {(!item.acknowledged || ack.locked) && <button type="button" disabled={ack.busy || ack.done} onClick={() => void ack.run({ worker, id: item.id })}>{ack.busy ? 'Recording…' : ack.locked ? 'Retry exact acknowledgement' : 'Acknowledge'}</button>}
    <ActionNotice error={seen.error || ack.error} done={seen.done || ack.done}>Message state recorded.</ActionNotice>
    {seen.retained?.uncertain && <p className="field-note">Seen recovery key: <code>{seen.retained.key}</code></p>}
    {ack.retained?.uncertain && <p className="field-note">Acknowledgement recovery key: <code>{ack.retained.key}</code></p>}
  </div>;
}
