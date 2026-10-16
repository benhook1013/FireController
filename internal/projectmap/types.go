package projectmap

type Phase struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Archived bool   `json:"archived"`
}
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}
type Editable struct {
	Kind        string  `json:"kind"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	State       string  `json:"state"`
	Where       string  `json:"where"`
	Next        string  `json:"next"`
	Trigger     string  `json:"trigger"`
	Phases      []Phase `json:"phases"`
	Links       []Link  `json:"links"`
	Archived    bool    `json:"archived"`
}
type Entry struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"project_id"`
	Revision    int64   `json:"revision"`
	Kind        string  `json:"kind"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	State       string  `json:"state"`
	Where       string  `json:"where"`
	Next        string  `json:"next"`
	Trigger     string  `json:"trigger"`
	Phases      []Phase `json:"phases"`
	Links       []Link  `json:"links"`
	Archived    bool    `json:"archived"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}
type Summary struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	State    string `json:"state"`
	Archived bool   `json:"archived"`
	Revision int64  `json:"revision"`
}
type HistorySummary struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
}
type Revision struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
	State     Entry  `json:"state"`
}
type Receipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}
type JobWorkstream struct {
	JobID           string `json:"job_id"`
	WorkstreamID    string `json:"workstream_id"`
	WorkstreamTitle string `json:"workstream_title"`
	Revision        int64  `json:"revision"`
}
type JobWorkstreamHistory struct {
	Revision        int64  `json:"revision"`
	WorkstreamID    string `json:"workstream_id"`
	WorkstreamTitle string `json:"workstream_title"`
	JobRevision     int64  `json:"job_revision"`
	ActorID         string `json:"actor_id"`
	CreatedAt       string `json:"created_at"`
}
type JobReceipt struct {
	JobID    string `json:"job_id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}
type Page[T any] struct {
	Items   []T    `json:"items"`
	Cursor  string `json:"cursor,omitempty"`
	HasMore bool   `json:"has_more"`
}
