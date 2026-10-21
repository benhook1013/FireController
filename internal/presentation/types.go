package presentation

import "github.com/benhook1013/FireController/internal/presentationmodel"

type Config = presentationmodel.Config

type State struct {
	ProjectID string            `json:"project_id"`
	Revision  int64             `json:"revision"`
	Name      string            `json:"name"`
	BrandName string            `json:"brand_name"`
	Tagline   string            `json:"tagline"`
	Headings  map[string]string `json:"headings"`
	Theme     map[string]string `json:"theme"`
	UpdatedAt string            `json:"updated_at"`
}

func (s State) Config() Config {
	return Config{Name: s.Name, BrandName: s.BrandName, Tagline: s.Tagline, Headings: s.Headings, Theme: s.Theme}
}
func state(c Config, project string, rev int64, at string) State {
	return State{project, rev, c.Name, c.BrandName, c.Tagline, c.Headings, c.Theme, at}
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
