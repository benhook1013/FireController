package reviews

// SubagentRecordRequest records a completed review supplied by its coordinating
// worker. Counts and finding identities are derived; no provider execution or
// provider-channel completion credit is created.
type SubagentRecordRequest struct {
	PullNumber     int64                  `json:"pull_number"`
	HeadSHA        string                 `json:"head_sha"`
	Reviewer       string                 `json:"reviewer"`
	Scope          string                 `json:"scope"`
	StartedAt      string                 `json:"started_at"`
	FinishedAt     string                 `json:"finished_at"`
	CoverageLimits []string               `json:"coverage_limits,omitempty"`
	Findings       []SubagentFindingInput `json:"findings"`
	Finalized      bool                   `json:"finalized,omitempty"`
}

type SubagentFindingInput struct {
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Severity    string `json:"severity"`
	Disposition string `json:"disposition,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type SubagentDecisionInput struct {
	Finding     int64  `json:"finding"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

type SubagentFixInput struct {
	Finding     int64   `json:"finding"`
	ExpectedSHA *string `json:"expected_sha"`
	FixSHA      string  `json:"fix_sha"`
	ProofNote   string  `json:"proof_note"`
}

type SubagentUpdateRequest struct {
	ExpectedRevision *int64                  `json:"expected_revision"`
	Decisions        []SubagentDecisionInput `json:"decisions,omitempty"`
	Fixes            []SubagentFixInput      `json:"fixes,omitempty"`
	Finalize         bool                    `json:"finalize,omitempty"`
}

type SubagentReceipt struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
}

type SubagentReport struct {
	ID         string        `json:"id"`
	ProjectID  string        `json:"project_id"`
	PullNumber int64         `json:"pull_number"`
	Revision   int64         `json:"revision"`
	Run        RunSummary    `json:"run"`
	Findings   []FindingView `json:"findings"`
}
