package presentation

import "github.com/benhook1013/FireController/internal/presentationmodel"

type Config = presentationmodel.Config

type State struct {
	ProjectID           string                          `json:"project_id"`
	Revision            int64                           `json:"revision"`
	Name                string                          `json:"name"`
	BrandName           string                          `json:"brand_name"`
	Tagline             string                          `json:"tagline"`
	Headings            map[string]string               `json:"headings"`
	Theme               map[string]string               `json:"theme"`
	ReviewGroups        []presentationmodel.ReviewGroup `json:"review_groups,omitempty"`
	SuggestedMaxPRFiles int64                           `json:"suggested_max_pr_files,omitempty"`
	WorkerOrder         []string                        `json:"worker_order,omitempty"`
	UpdatedAt           string                          `json:"updated_at"`
}

func (s State) Config() Config {
	return Config{Name: s.Name, BrandName: s.BrandName, Tagline: s.Tagline, Headings: s.Headings, Theme: s.Theme, ReviewGroups: s.ReviewGroups, WorkerOrder: s.WorkerOrder, SuggestedMaxPRFiles: s.SuggestedMaxPRFiles}
}
func state(c Config, project string, rev int64, at string) State {
	return State{ProjectID: project, Revision: rev, Name: c.Name, BrandName: c.BrandName, Tagline: c.Tagline, Headings: c.Headings, Theme: c.Theme, UpdatedAt: at, ReviewGroups: c.ReviewGroups, WorkerOrder: c.WorkerOrder, SuggestedMaxPRFiles: c.SuggestedMaxPRFiles}
}

type Snapshot struct {
	Presentation State  `json:"presentation"`
	EventCursor  string `json:"event_cursor"`
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
	State     State  `json:"state"`
}
type Receipt struct {
	ProjectID string `json:"project_id"`
	Revision  int64  `json:"revision"`
	Outcome   string `json:"outcome"`
}
type Page struct {
	Items   []HistorySummary `json:"items"`
	Cursor  string           `json:"cursor,omitempty"`
	HasMore bool             `json:"has_more"`
}
