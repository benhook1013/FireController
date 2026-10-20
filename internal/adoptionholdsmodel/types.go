// Package adoptionholdsmodel defines inert carried obligations without provider dependencies.
package adoptionholdsmodel

type Source struct {
	Namespace      string `json:"namespace"`
	RepositoryID   int64  `json:"repository_id"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
}
type Obligation struct {
	SourceKey           string   `json:"source_key"`
	Kind                string   `json:"kind"`
	AffectedPullNumbers []int64  `json:"affected_pull_numbers"`
	Summary             string   `json:"summary"`
	Evidence            []string `json:"evidence"`
	OriginalStatus      string   `json:"original_status"`
}
type Batch struct {
	Version          int          `json:"version"`
	Source           Source       `json:"source"`
	ExpectedRevision *int64       `json:"expected_revision"`
	Obligations      []Obligation `json:"obligations"`
}
type ApplyRequest struct {
	Batch         Batch  `json:"batch"`
	PreviewSHA256 string `json:"preview_sha256"`
}
type ResolveRequest struct {
	ExpectedRevision *int64   `json:"expected_revision"`
	Reason           string   `json:"reason"`
	Evidence         []string `json:"evidence"`
}
type Preview struct {
	ProjectID      string `json:"project_id"`
	SourceRevision int64  `json:"source_revision"`
	Selected       int    `json:"selected"`
	New            int    `json:"new"`
	Unchanged      int    `json:"unchanged"`
	PayloadSHA256  string `json:"payload_sha256"`
	PreviewSHA256  string `json:"preview_sha256"`
}
type Receipt struct {
	Outcome   string `json:"outcome"`
	Revision  int64  `json:"revision"`
	New       int    `json:"new"`
	Unchanged int    `json:"unchanged"`
}
type SourceState struct {
	ProjectID string `json:"project_id"`
	Source    Source `json:"source"`
	Revision  int64  `json:"revision"`
}
type Resolution struct {
	Revision   int64    `json:"revision"`
	Reason     string   `json:"reason"`
	Evidence   []string `json:"evidence"`
	ActorID    string   `json:"actor_id"`
	WorkerID   string   `json:"worker_id"`
	ResolvedAt string   `json:"resolved_at"`
}
type Detail struct {
	ID             string      `json:"id"`
	ProjectID      string      `json:"project_id"`
	Source         Source      `json:"source"`
	SourceRevision int64       `json:"source_revision"`
	Original       Obligation  `json:"original"`
	Status         string      `json:"status"`
	Revision       int64       `json:"revision"`
	ActorID        string      `json:"actor_id"`
	WorkerID       string      `json:"worker_id"`
	CreatedAt      string      `json:"created_at"`
	Resolution     *Resolution `json:"resolution"`
}
type ListItem struct {
	ID                  string  `json:"id"`
	Namespace           string  `json:"namespace"`
	SourceKey           string  `json:"source_key"`
	Kind                string  `json:"kind"`
	Status              string  `json:"status"`
	Revision            int64   `json:"revision"`
	AffectedPullNumbers []int64 `json:"affected_pull_numbers"`
}
type Page struct {
	Items      []ListItem `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}
