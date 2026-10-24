export interface PageOptions {limit?: number; cursor?: string}
export interface Page<T> {items: T[]; cursor?: string; has_more: boolean}
export interface Project {handle?: string; id: string; name: string; github_repository_id: number; repository: string}
export interface Worker {id: string; project_id: string; name: string}
export interface JobIdentity {id: string; title: string; worker_id: string; status: 'active'|'parked'|'blocked'|'completed'; revision: number}
export interface JobSummary extends JobIdentity {progress_summary: string; blocker: string; progress_truncated: boolean; blocker_truncated: boolean}
export interface JobProgress {summary: string; next: string; blocker: string; evidence: string[]}
export interface JobProgressEntry {id: string; project_id: string; job_id: string; job_revision: number; sequence: number; actor_id: string; worker_id: string; progress: JobProgress; created_at: string}
export interface JobChecklistSummary {revision: number; remaining: ChecklistItem[]; completed: number; total: number}
export interface Job extends JobIdentity {project_id: string; brief: string; created_at: string; updated_at: string; progress: JobProgress; checklist: JobChecklistSummary; notes: Page<NoteSummary>}
export interface DeliveryWorker extends Worker {primary_job_id: string; paused: boolean; pause_reason: string; pause_reason_truncated: boolean}
export interface DeliveryProgress {summary: string; next: string; blocker: string; summary_truncated: boolean; next_truncated: boolean; blocker_truncated: boolean}
export interface DeliveryChecklist {revision: number; job_revision: number; total: number; done: number; created_at: string}
export interface JobPublicSummary {job_id: string; job_revision: number; revision: number; title: string; body: string}
export interface DeliveryJob extends JobIdentity {summary?: JobPublicSummary;worker_name: string; brief_preview: string; brief_preview_truncated: boolean; updated_at: string; progress: DeliveryProgress; checklist: DeliveryChecklist|null}
export interface DeliverySnapshot {workers: Page<DeliveryWorker>; jobs: Page<DeliveryJob>}
export interface DeliveryOptions {worker_limit?: number; job_limit?: number; worker_cursor?: string; job_cursor?: string}
export interface Checkpoint {id: string; project_id: string; job_id: string; job_revision: number; sequence: number; actor_id: string; worker_id: string; done: string; next: string; blocker: string; evidence: string[]; created_at: string}
export interface LatestCheckpoint {job_id: string; job_revision: number; checkpoint: Checkpoint|null}
export interface JobUpdate {id: string; project_id: string; job_id: string; job_revision: number; sequence: number; actor_id: string; worker_id: string; body: string; evidence: string[]; created_at: string}
export interface ChecklistItem {id: string; text: string; done: boolean}
export interface JobChecklist {job_id: string; job_revision: number; revision: number; items: ChecklistItem[]}
export interface ChecklistHistoryEntry {revision: number; job_revision: number; actor_id: string; worker_id: string; items: ChecklistItem[]; created_at: string}
export type ProjectMapKind = 'workstream'|'return_point'|'section';
export interface ProjectMapPhase {id: string; name: string; state: string; archived: boolean}
export interface ProjectMapLink {label: string; url: string}
export interface ProjectMapSummary {id: string; kind: ProjectMapKind; title: string; state: string; archived: boolean; revision: number}
export interface ProjectMapEntry extends ProjectMapSummary {project_id: string; description: string; where: string; next: string; trigger: string; phases: ProjectMapPhase[]; links: ProjectMapLink[]; created_at: string; updated_at: string; position?: number}
export interface ProjectMapHistorySummary {revision: number; actor_id: string; created_at: string}
export interface ProjectMapRevision extends ProjectMapHistorySummary {state: ProjectMapEntry}
export interface ProjectMapOptions extends PageOptions {kind?: ProjectMapKind; archived?: boolean}
export interface JobWorkstream {job_id: string; workstream_id: string; workstream_title: string; revision: number}
export interface JobWorkstreamHistoryEntry {revision: number; workstream_id: string; workstream_title: string; job_revision: number; actor_id: string; created_at: string}
export interface JobOptions extends PageOptions {worker_id?: string; status?: JobSummary['status']|'open'}
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
export type ReviewAttemptState = ReviewOutcome|'preparing'|'held';
export interface ReviewAcceptedSeverity {critical: number; major: number; minor: number; trivial: number; unknown: number; complete: boolean}
export interface ReviewRunSummary {accepted_severity?: ReviewAcceptedSeverity; source: string; id: string; pull_number: number; source_run_id: string; channel: ReviewChannel; head_sha: string; reviewer: string; scope: string; coverage_limits: string[]; outcome: ReviewOutcome; attributable: boolean; complete: boolean; started_at: string; finished_at: string; found: number; accepted: number; routed: number; finalized: boolean; fixed: number}
export interface ReviewAttemptSummary {reason?: string; source_attempt_id: string; channel: ReviewChannel; head_sha: string; state: ReviewAttemptState; started_at: string; finished_at: string; duration_seconds: number | null; source_run_id: string; trigger_id: string; provider_review_id: string}
export interface ReviewPullDiff {files: number; additions: number; deletions: number}
export type ReviewRunCounts = Record<ReviewChannel, number>;
export interface ReviewPullObservationHealth {state: 'not_enrolled'|'unavailable'|'waiting'|'current'|'comparison_changed'|'overdue'|'retrying'; checked_at?:string; last_success_at?:string; next_check_at?:string; error?:string; ci_checked_at?:string; ci_error?:string; hosted_checked_at?:string; hosted_error?:string}
export interface ReviewObserverHealth {state: 'running'|'unavailable'|'unconfigured'; started_at?:string}
export interface ReviewPullSummary {observation?:ReviewPullObservationHealth;queue_position?: number; diff?: ReviewPullDiff; is_draft?: boolean; merged_at?: string; number: number; title: string; state: 'open'|'merged'|'closed'|'unknown'; head_sha: string; base_ref: string; base_sha: string; ci_state: 'unknown'|'pending'|'success'|'failure'; mergeability: 'unknown'|'mergeable'|'conflicting'; complete: boolean; run_count: number; run_counts: ReviewRunCounts; attempt_count: number; recent_runs: ReviewRunSummary[]; latest_attempts: ReviewAttemptSummary[]}
export interface ReviewFindingRoute {id:string;target_pull_number:number;revision:number;state:"active"|"superseded";disposition:"unadjudicated"|"accepted"|"rejected";reason:string;actor:string;decided_at:string;fix_sha:string;proof_note:string;fixed_at:string}
export interface ReviewFinding {display_severity?: ReviewFinding['severity']; route?:ReviewFindingRoute;source_finding_id: string; title: string; detail: string; severity: 'info'|'low'|'medium'|'high'|'critical'|'unknown'; disposition: 'unadjudicated'|'accepted'|'routed'|'rejected'; decision_reason: string; actor: string; decided_at: string; fix_sha: string; proof_note: string; resolved_at: string}
export interface ReviewSnapshot {project_id: string; revision: number; snapshot_id: string; source: string; observed_at: string; recorded_at: string}
export interface ReviewQueue extends ReviewSnapshot, Page<ReviewPullSummary> {observer?:ReviewObserverHealth;configured: boolean; head_position?: number}
export interface ReviewPull extends ReviewSnapshot {pull: ReviewPullSummary}
export interface ReviewRunOptions extends PageOptions {snapshot?: string; channel?: ReviewChannel; view?: 'timeline'}
export interface ReviewRuns extends ReviewSnapshot, Page<ReviewRunSummary> {}
export interface ReviewAttemptItem extends ReviewAttemptSummary {source: string}
export interface ReviewAttemptOptions extends PageOptions {snapshot?: string}
export interface ReviewAttempts extends ReviewSnapshot, Page<ReviewAttemptItem> {}
export interface ReviewRunDetail extends ReviewSnapshot {pull_number: number; run: ReviewRunSummary; findings: ReviewFinding[]}

export interface ReviewAnchor {repository_id: number; repository: string; pull_number: number; parent_pull_number: number; candidate_sha: string; base_sha: string; parent_sha: string; merge_base_sha: string; patch_sha256: string; base_ref: string; observed_at: string}
export interface ReviewPolicyAssessmentChannel {channel: 'cli'|'hosted'; covered: boolean; pending: boolean; rounds_configured?: boolean; rounds_complete?: boolean}
export interface ReviewPolicyAssessment {source_obligations_pending?:number;incoming_routes_pending?:number;incoming_routes_capacity?:boolean;reason?:string;project_id: string; evidence_id: string; anchor: ReviewAnchor; source: 'captured_comparison'; policy_revision: number; evaluated_at: string; state: 'sufficient'|'held'|'unconfigured'; channels: ReviewPolicyAssessmentChannel[]}

export interface ReviewGroup {title: string; pull_numbers: number[]}
export interface Presentation {project_id: string; revision: number; name: string; brand_name: string; tagline: string; headings: Partial<Record<import('./presentation').HeadingKey, string>>; theme: Partial<Record<import('./presentation').ThemeToken, string>>; review_groups?: ReviewGroup[]; updated_at: string}
export interface PresentationSnapshot {presentation: Presentation; event_cursor: string}
export interface PresentationRevision {revision: number; actor_id: string; created_at: string; state: Presentation}
export interface ProjectMetadataEvent {event_type: import('./project-event-types').ProjectEventType; resource_id: string; revision: number; event_cursor: string}
export interface ProjectMetadataPage {items: ProjectMetadataEvent[]; event_cursor: string; has_more: boolean}

export interface WorkerPrimary {worker_id: string; revision: number; job_id: string}
export interface WorkerPrimaryHistoryEntry extends WorkerPrimary {actor_id: string; actor_worker_id: string; reason: 'selected'|'cleared'|'job_reassigned'; created_at: string}
export interface WorkerPause {worker_id: string; revision: number; paused: boolean; reason: string}
export interface WorkerPauseHistoryEntry extends WorkerPause {actor_id: string; actor_worker_id: string; created_at: string}
export type NoteKind = 'reminder'|'source';
export type NoteStatus = 'pending'|'consumed'|'dismissed';
export interface NoteOptions extends PageOptions {worker_id?: string; job_id?: string; phase?: string; kind?: NoteKind; status?: NoteStatus|'all'; include_assigned_jobs?: boolean}
export interface NoteSummary {id: string; project_id: string; revision: number; worker_id: string; job_id: string; phase: string; kind: NoteKind; status: NoteStatus; created_at: string; updated_at: string; body_preview: string; body_truncated: boolean}
export interface Note {id: string; project_id: string; revision: number; worker_id: string; job_id: string; phase: string; kind: NoteKind; status: NoteStatus; created_at: string; updated_at: string; body: string; status_reason: string}
export interface NoteHistorySummary extends NoteSummary {actor_id: string; actor_worker_id: string; recorded_at: string}
export interface NoteRevision extends Note {actor_id: string; actor_worker_id: string; recorded_at: string}
