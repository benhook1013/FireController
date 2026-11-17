package legacy

// Fixed source-column inventories from the copied legacy controller sources.
// These identify inspected tables; they are not a complete DDL/content validator.
type tableSpec struct {
	name, domain string
	columns      []string
}

var tables = []tableSpec{
	{"controller_metadata", "controller", []string{"singleton", "data_model_version", "min_writer_build"}},
	{"review_state", "controller", []string{"singleton", "state_json"}},
	{"review_records_metadata", "reviews", []string{"singleton", "records_schema_version", "controller_schema_version", "controller_data_model_version", "min_writer_build"}},
	{"review_runs", "reviews", []string{"run_id", "source_pr", "channel", "source_head", "reviewer", "scope", "coverage_limits_json", "import_payload_json", "outcome", "attributable", "started_at", "finished_at", "found_count", "accepted_count", "routed_count", "finalized", "finalized_at"}},
	{"findings", "reviews", []string{"finding_id", "source_pr", "source_channel", "source_finding_key", "first_seen_at"}},
	{"finding_observations", "reviews", []string{"run_id", "finding_id", "source_pr", "source_channel", "title", "detail", "disposition", "route_id", "display_severity"}},
	{"routes", "reviews", []string{"route_id", "finding_id", "source_pr", "source_channel", "target_pr", "status", "created_at", "updated_at"}},
	{"route_target_history", "reviews", []string{"sequence", "route_id", "target_pr", "changed_at", "actor", "reason"}},
	{"decisions", "reviews", []string{"decision_id", "decision_scope", "run_id", "finding_id", "route_id", "decision_pr", "target_pr", "decision", "actor", "reason", "decided_at"}},
	{"resolutions", "reviews", []string{"resolution_id", "route_id", "resolution_pr", "outcome", "actor", "proof_or_reason", "resolved_at"}},
	{"source_finding_resolutions", "reviews", []string{"resolution_id", "run_id", "finding_id", "source_pr", "source_channel", "outcome", "fix_sha", "actor", "proof_note", "resolved_at"}},
	{"source_finding_resolution_corrections", "reviews", []string{"sequence", "correction_id", "resolution_id", "expected_fix_sha", "corrected_fix_sha", "actor", "reason", "proof_note", "corrected_at"}},
	{"review_attempts", "reviews", []string{"attempt_id", "source_pr", "channel", "candidate_sha", "state", "started_at", "finished_at", "duration_seconds", "exit_status", "trigger_id", "provider_review_id", "checkpoint_id", "run_id", "diagnostic", "metadata_json"}},
	{"review_artifacts", "reviews", []string{"attempt_id", "kind", "content", "source_sha256", "redactions"}},
	{"source_decision_corrections", "reviews", []string{"sequence", "correction_id", "supersedes_id", "run_id", "finding_id", "decision", "target_pr", "actor", "reason", "decided_at"}},
	{"provider_origins", "reviews", []string{"repository", "source_pr", "channel", "provider_id", "checkpoint_id", "checkpoint_fingerprint", "run_id"}},
	{"imported_artifacts", "reviews", []string{"run_id", "kind", "content", "source_sha256", "redactions"}},
	{"historical_provider_gaps", "reviews", []string{"repository", "source_pr", "channel", "checkpoint_id", "checkpoint_fingerprint", "checkpoint_json", "checkpoint_source_sha256", "checkpoint_redactions", "missing_reason"}},
	{"historical_gap_artifacts", "reviews", []string{"repository", "source_pr", "checkpoint_id", "kind", "content", "source_sha256", "redactions"}},
	{"job_metadata", "jobs", []string{"singleton", "jobs_schema_version", "controller_schema_version", "controller_data_model_version"}},
	{"jobs", "jobs", []string{"id", "name", "worker", "workstream_id", "title", "status", "is_primary", "revision", "brief_revision", "summary", "progress", "blocker", "checklist_json", "chat_id", "created_at", "updated_at"}},
	{"job_revisions", "jobs", []string{"job_id", "revision", "brief_revision", "created_at", "state_json"}},
	{"job_briefs", "jobs", []string{"job_id", "brief_revision", "created_at", "brief"}},
	{"job_updates", "jobs", []string{"sequence", "job_id", "created_at", "kind", "body"}},
	{"job_checkpoints", "jobs", []string{"sequence", "job_id", "created_at", "done", "next_steps", "blocker", "pointers_json"}},
	{"job_notes", "jobs", []string{"id", "body", "worker", "job", "phase", "kind", "status", "dismissal_reason", "revision", "created_at", "updated_at"}},
	{"job_note_revisions", "jobs", []string{"note_id", "revision", "created_at", "state_json"}},
	{"job_worker_state", "jobs", []string{"worker", "paused", "reason", "updated_at"}},
	{"job_worker_state_history", "jobs", []string{"sequence", "worker", "created_at", "paused", "reason"}},
	{"job_imports", "jobs", []string{"manifest_fingerprint", "created_at", "manifest_json", "source_contents_json", "provenance_json"}},
	{"inbox_metadata", "inbox", []string{"singleton", "inbox_schema_version"}},
	{"inbox_messages", "inbox", []string{"id", "recipient", "body", "author", "job", "pr", "reply_to", "created_at", "seen_at", "acknowledged_at"}},
	{"map_metadata", "map", []string{"singleton", "map_schema_version", "controller_schema_version", "controller_data_model_version"}},
	{"map_workstreams", "map", []string{"id", "revision", "state", "now_text", "milestone", "phase_states_json", "updated_at"}},
	{"map_return_points", "map", []string{"id", "revision", "state", "updated_at"}},
	{"map_revisions", "map", []string{"record_type", "record_id", "revision", "created_at", "snapshot_json"}},
	{"map_imports", "map", []string{"source_fingerprint", "created_at", "status_source", "status_sha256", "progress_source", "progress_sha256", "original_values_json", "manifest_json", "provenance_json"}},
}
