// Package reviews implements a project-scoped, provider-free read model for
// administrator-imported review observations.
package reviews

// ObservationRequest replaces the current immutable review observation for a
// project. ExpectedRevision is a pointer so the required zero initial revision
// is distinct from an omitted field.
type ObservationRequest struct {
	ExpectedRevision *int64        `json:"expected_revision"`
	RepositoryID     int64         `json:"repository_id"`
	Source           string        `json:"source"`
	ObservedAt       string        `json:"observed_at"`
	Queue            []PullInput   `json:"queue"`
	Retired          []RetiredPull `json:"retired"`
}

type PullInput struct {
	Diff         *PullDiff  `json:"diff,omitempty"`
	IsDraft      *bool      `json:"is_draft,omitempty"`
	MergedAt     string     `json:"merged_at,omitempty"`
	Number       int64      `json:"number"`
	Title        string     `json:"title"`
	State        string     `json:"state"`
	HeadSHA      string     `json:"head_sha"`
	BaseRef      string     `json:"base_ref"`
	BaseSHA      string     `json:"base_sha"`
	CIState      string     `json:"ci_state"`
	Mergeability string     `json:"mergeability"`
	Complete     bool       `json:"complete"`
	Runs         []RunInput `json:"runs"`
	Attempts     []Attempt  `json:"attempts"`
}

type RetiredPull struct {
	Number int64  `json:"number"`
	Reason string `json:"reason"`
}

type RunInput struct {
	SourceRunID    string    `json:"source_run_id"`
	Channel        string    `json:"channel"`
	HeadSHA        string    `json:"head_sha"`
	Reviewer       string    `json:"reviewer"`
	Scope          string    `json:"scope"`
	Outcome        string    `json:"outcome"`
	Attributable   bool      `json:"attributable"`
	Complete       bool      `json:"complete"`
	StartedAt      string    `json:"started_at"`
	FinishedAt     string    `json:"finished_at"`
	Found          int64     `json:"found"`
	Accepted       int64     `json:"accepted"`
	Routed         int64     `json:"routed"`
	Finalized      bool      `json:"finalized"`
	CoverageLimits []string  `json:"coverage_limits"`
	Findings       []Finding `json:"findings"`
}

type Attempt struct {
	SourceAttemptID  string `json:"source_attempt_id"`
	Channel          string `json:"channel"`
	HeadSHA          string `json:"head_sha"`
	State            string `json:"state"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
	DurationSeconds  *int64 `json:"duration_seconds"`
	SourceRunID      string `json:"source_run_id"`
	TriggerID        string `json:"trigger_id"`
	ProviderReviewID string `json:"provider_review_id"`
}

// FindingRoute is the retained receiving-side state of a same-project route.
// Source decision fields remain on Finding; receiver proof is never source proof.
type FindingRoute struct {
	ID               string `json:"id"`
	TargetPullNumber int64  `json:"target_pull_number"`
	Revision         int64  `json:"revision"`
	State            string `json:"state"`
	Disposition      string `json:"disposition"`
	Reason           string `json:"reason"`
	Actor            string `json:"actor"`
	DecidedAt        string `json:"decided_at"`
	FixSHA           string `json:"fix_sha"`
	ProofNote        string `json:"proof_note"`
	FixedAt          string `json:"fixed_at"`
}

type Finding struct {
	Route           *FindingRoute `json:"route,omitempty"`
	SourceFindingID string        `json:"source_finding_id"`
	Title           string        `json:"title"`
	Detail          string        `json:"detail"`
	Severity        string        `json:"severity"`
	Disposition     string        `json:"disposition"`
	DecisionReason  string        `json:"decision_reason"`
	Actor           string        `json:"actor"`
	DecidedAt       string        `json:"decided_at"`
	FixSHA          string        `json:"fix_sha"`
	ProofNote       string        `json:"proof_note"`
	ResolvedAt      string        `json:"resolved_at"`
}

type Receipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type AttemptSummary struct {
	SourceAttemptID  string `json:"source_attempt_id"`
	Channel          string `json:"channel"`
	HeadSHA          string `json:"head_sha"`
	State            string `json:"state"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
	DurationSeconds  *int64 `json:"duration_seconds"`
	SourceRunID      string `json:"source_run_id"`
	TriggerID        string `json:"trigger_id"`
	ProviderReviewID string `json:"provider_review_id"`
}

type RunSummary struct {
	Source         string   `json:"source"`
	ID             string   `json:"id"`
	PullNumber     int64    `json:"pull_number"`
	SourceRunID    string   `json:"source_run_id"`
	Channel        string   `json:"channel"`
	HeadSHA        string   `json:"head_sha"`
	Reviewer       string   `json:"reviewer"`
	Scope          string   `json:"scope"`
	Outcome        string   `json:"outcome"`
	Attributable   bool     `json:"attributable"`
	Complete       bool     `json:"complete"`
	StartedAt      string   `json:"started_at"`
	FinishedAt     string   `json:"finished_at"`
	Found          int64    `json:"found"`
	Accepted       int64    `json:"accepted"`
	Routed         int64    `json:"routed"`
	Finalized      bool     `json:"finalized"`
	Fixed          int64    `json:"fixed"`
	CoverageLimits []string `json:"coverage_limits"`
}

type PullSummary struct {
	Diff           *PullDiff        `json:"diff,omitempty"`
	IsDraft        *bool            `json:"is_draft,omitempty"`
	MergedAt       string           `json:"merged_at,omitempty"`
	Number         int64            `json:"number"`
	Title          string           `json:"title"`
	State          string           `json:"state"`
	HeadSHA        string           `json:"head_sha"`
	BaseRef        string           `json:"base_ref"`
	BaseSHA        string           `json:"base_sha"`
	CIState        string           `json:"ci_state"`
	Mergeability   string           `json:"mergeability"`
	Complete       bool             `json:"complete"`
	RunCount       int64            `json:"run_count"`
	AttemptCount   int64            `json:"attempt_count"`
	RecentRuns     []RunSummary     `json:"recent_runs"`
	LatestAttempts []AttemptSummary `json:"latest_attempts"`
}

type QueueResponse struct {
	ProjectID  string        `json:"project_id"`
	Configured bool          `json:"configured"`
	Revision   int64         `json:"revision"`
	SnapshotID string        `json:"snapshot_id"`
	Source     string        `json:"source"`
	ObservedAt string        `json:"observed_at"`
	RecordedAt string        `json:"recorded_at"`
	Items      []PullSummary `json:"items"`
	Cursor     string        `json:"cursor,omitempty"`
	HasMore    bool          `json:"has_more"`
}

type PullResponse struct {
	ProjectID  string      `json:"project_id"`
	Revision   int64       `json:"revision"`
	SnapshotID string      `json:"snapshot_id"`
	Source     string      `json:"source"`
	ObservedAt string      `json:"observed_at"`
	RecordedAt string      `json:"recorded_at"`
	Pull       PullSummary `json:"pull"`
}

// AttemptHistoryItem keeps the original producer distinct from the snapshot
// publisher. External attempt IDs need only be unique within that producer.
type AttemptHistoryItem struct {
	AttemptSummary
	Source string `json:"source"`
}

type AttemptsResponse struct {
	ProjectID  string               `json:"project_id"`
	Revision   int64                `json:"revision"`
	SnapshotID string               `json:"snapshot_id"`
	Source     string               `json:"source"`
	ObservedAt string               `json:"observed_at"`
	RecordedAt string               `json:"recorded_at"`
	Items      []AttemptHistoryItem `json:"items"`
	Cursor     string               `json:"cursor,omitempty"`
	HasMore    bool                 `json:"has_more"`
}

type RunsResponse struct {
	ProjectID  string       `json:"project_id"`
	Revision   int64        `json:"revision"`
	SnapshotID string       `json:"snapshot_id"`
	Source     string       `json:"source"`
	ObservedAt string       `json:"observed_at"`
	RecordedAt string       `json:"recorded_at"`
	Items      []RunSummary `json:"items"`
	Cursor     string       `json:"cursor,omitempty"`
	HasMore    bool         `json:"has_more"`
}

type RunDetailResponse struct {
	ProjectID  string     `json:"project_id"`
	Revision   int64      `json:"revision"`
	SnapshotID string     `json:"snapshot_id"`
	Source     string     `json:"source"`
	ObservedAt string     `json:"observed_at"`
	RecordedAt string     `json:"recorded_at"`
	PullNumber int64      `json:"pull_number"`
	Run        RunSummary `json:"run"`
	Findings   []Finding  `json:"findings"`
}
