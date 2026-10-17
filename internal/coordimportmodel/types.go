// Package coordimportmodel contains native prepared-history wire types, without
// storage, network or legacy source dependencies.
package coordimportmodel

type PreparedJob struct {
	SourceKey      string               `json:"source_key"`
	Title          string               `json:"title"`
	Brief          string               `json:"brief"`
	WorkerID       string               `json:"worker_id"`
	Status         string               `json:"status"`
	CreatedAt      string               `json:"created_at"`
	UpdatedAt      string               `json:"updated_at"`
	SourceRevision int64                `json:"source_revision"`
	Checkpoints    []PreparedCheckpoint `json:"checkpoints"`
}

type PreparedCheckpoint struct {
	SourceKey string   `json:"source_key"`
	ActorID   string   `json:"actor_id"`
	WorkerID  string   `json:"worker_id"`
	Done      string   `json:"done"`
	Next      string   `json:"next"`
	Blocker   string   `json:"blocker"`
	CreatedAt string   `json:"created_at"`
	Evidence  []string `json:"evidence"`
}

type Batch struct {
	Version          int           `json:"version"`
	RepositoryID     int64         `json:"repository_id"`
	ExpectedRevision *int64        `json:"expected_revision"`
	Source           string        `json:"source"`
	SourceSHA256     string        `json:"source_sha256"`
	Jobs             []PreparedJob `json:"jobs"`
}

type ApplyRequest struct {
	PreviewSHA256 string `json:"preview_sha256"`
	Batch         Batch  `json:"batch"`
}

type Counts struct {
	Jobs          int `json:"jobs"`
	Checkpoints   int `json:"checkpoints"`
	NewJobs       int `json:"new_jobs"`
	UnchangedJobs int `json:"unchanged_jobs"`
}

type Preview struct {
	ProjectID        string `json:"project_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	PayloadSHA256    string `json:"payload_sha256"`
	PreviewSHA256    string `json:"preview_sha256"`
	Counts           Counts `json:"counts"`
}

type Mapping struct {
	SourceKey string `json:"source_key"`
	JobID     string `json:"job_id"`
}

type Receipt struct {
	Revision int64     `json:"revision"`
	Outcome  string    `json:"outcome"`
	Counts   Counts    `json:"counts"`
	Mappings []Mapping `json:"mappings"`
}
