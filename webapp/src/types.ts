export interface PageOptions {limit?: number; cursor?: string}
export interface Page<T> {items: T[]; cursor?: string; has_more: boolean}
export interface Project {id: string; name: string; github_repository_id: number; repository: string}
export interface Worker {id: string; project_id: string; name: string}
export interface JobSummary {id: string; title: string; worker_id: string; status: 'active'|'parked'|'blocked'|'completed'; revision: number}
export interface Job extends JobSummary {project_id: string; brief: string; created_at: string; updated_at: string}
export interface JobOptions extends PageOptions {worker_id?: string; status?: JobSummary['status']}
export interface InboxOptions extends PageOptions {unread?: boolean; unacknowledged?: boolean}
export interface InboxItem {id: string; conversation_id: string; sender_worker_id: string; recipient_worker_id: string; preview: string; preview_truncated: boolean; seen: boolean; acknowledged: boolean; created_at: string}
export interface Message {id: string; conversation_id: string; sender_worker_id: string; recipient_worker_id: string; reply_to_message_id?: string; body: string; created_at: string}
export interface ConversationPage extends Page<Message> {conversation_id: string; root_message_id: string}
export interface InboxSnapshot {inbox: Page<InboxItem>; event_cursor: string}
export interface MessageEvent {sequence: number; project_id: string; type: 'message.sent'|'message.seen'|'message.acknowledged'; resource_id: string; revision: number; actor_id: string; payload: {conversation_id: string; recipient_worker_id: string; sender_worker_id?: string}; created_at: string}
export interface MessageEventPage extends Page<MessageEvent> {cursor: string; timed_out: boolean}
export type LiveStatus = 'connecting'|'live'|'reconnecting'|'closed';
export interface WatchCallbacks {snapshot(value: InboxSnapshot): void|Promise<void>; events(value: MessageEventPage): void|Promise<void>; status(value: LiveStatus): void}
