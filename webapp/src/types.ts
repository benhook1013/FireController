export interface PageOptions {limit?: number; cursor?: string}
export interface Page<T> {items: T[]; cursor?: string; has_more: boolean}
export interface Project {id: string; name: string; github_repository_id: number; repository: string}
export interface Worker {id: string; project_id: string; name: string}
export interface JobSummary {id: string; title: string; worker_id: string; status: 'active'|'parked'|'blocked'|'completed'; revision: number}
export interface Job extends JobSummary {project_id: string; brief: string; created_at: string; updated_at: string}
export interface Checkpoint {id: string; project_id: string; job_id: string; job_revision: number; sequence: number; actor_id: string; worker_id: string; done: string; next: string; blocker: string; evidence: string[]; created_at: string}
export interface LatestCheckpoint {job_id: string; job_revision: number; checkpoint: Checkpoint|null}
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

export type ReviewChannel = 'cli'|'hosted'|'manual'|'subagent';
export type ReviewOutcome = 'completed'|'active'|'prepared'|'posting'|'failed'|'cancelled'|'rate_limited'|'uncertain'|'unknown';
export interface ReviewRunSummary {source: string; id: string; pull_number: number; source_run_id: string; channel: ReviewChannel; head_sha: string; reviewer: string; scope: string; coverage_limits: string[]; outcome: ReviewOutcome; attributable: boolean; complete: boolean; started_at: string; finished_at: string; found: number; accepted: number; routed: number; finalized: boolean; fixed: number}
export interface ReviewAttemptSummary {source_attempt_id: string; channel: ReviewChannel; head_sha: string; state: ReviewOutcome; started_at: string; finished_at: string; duration_seconds: number | null; source_run_id: string; trigger_id: string; provider_review_id: string}
export interface ReviewPullSummary {number: number; title: string; state: 'open'|'merged'|'closed'|'unknown'; head_sha: string; base_ref: string; base_sha: string; ci_state: 'unknown'|'pending'|'success'|'failure'; mergeability: 'unknown'|'mergeable'|'conflicting'; complete: boolean; run_count: number; attempt_count: number; recent_runs: ReviewRunSummary[]; latest_attempts: ReviewAttemptSummary[]}
export interface ReviewFinding {source_finding_id: string; title: string; detail: string; severity: 'info'|'low'|'medium'|'high'|'critical'|'unknown'; disposition: 'unadjudicated'|'accepted'|'routed'|'rejected'; decision_reason: string; actor: string; decided_at: string; fix_sha: string; proof_note: string; resolved_at: string}
export interface ReviewSnapshot {project_id: string; revision: number; snapshot_id: string; source: string; observed_at: string; recorded_at: string}
export interface ReviewQueue extends ReviewSnapshot, Page<ReviewPullSummary> {configured: boolean}
export interface ReviewPull extends ReviewSnapshot {pull: ReviewPullSummary; attempts: ReviewAttemptSummary[]}
export interface ReviewRunOptions extends PageOptions {snapshot?: string}
export interface ReviewRuns extends ReviewSnapshot, Page<ReviewRunSummary> {}
export interface ReviewRunDetail extends ReviewSnapshot {pull_number: number; run: ReviewRunSummary; findings: ReviewFinding[]}
