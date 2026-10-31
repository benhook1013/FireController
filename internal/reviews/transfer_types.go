package reviews

// TransferRequest adds a bounded batch without replacing omitted history.
// Publisher records transfer provenance; group sources retain original producers.
type TransferRequest struct {
	Version          int             `json:"version"`
	ExpectedRevision *int64          `json:"expected_revision"`
	RepositoryID     int64           `json:"repository_id"`
	Publisher        string          `json:"publisher"`
	Groups           []TransferGroup `json:"groups"`
}

type TransferGroup struct {
	Source string      `json:"source"`
	Pulls  []PullInput `json:"pulls"`
}

type TransferCounts struct {
	Pulls    int64 `json:"pulls"`
	Runs     int64 `json:"runs"`
	Attempts int64 `json:"attempts"`
	Findings int64 `json:"findings"`
}

// TransferPreview binds normalized content and its destination, not approval.
type TransferPreview struct {
	Version          int            `json:"version"`
	ProjectID        string         `json:"project_id"`
	RepositoryID     int64          `json:"repository_id"`
	Publisher        string         `json:"publisher"`
	ExpectedRevision int64          `json:"expected_revision"`
	PayloadSHA256    string         `json:"payload_sha256"`
	PreviewSHA256    string         `json:"preview_sha256"`
	Counts           TransferCounts `json:"counts"`
	ImportedHistory  bool           `json:"imported_history"`
	NativeCoverage   bool           `json:"native_coverage"`
	Coverage         string         `json:"coverage"`
}

type TransferApplyRequest struct {
	PreviewSHA256 string          `json:"preview_sha256"`
	Batch         TransferRequest `json:"batch"`
}
