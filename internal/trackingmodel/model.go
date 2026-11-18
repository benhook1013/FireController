// Package trackingmodel holds the wire types shared by the tracking service
// and its clients. It has no storage or transport dependencies.
package trackingmodel

// Statuses is the fixed item status enum, in display order.
var Statuses = []string{"planned", "active", "blocked", "deferred", "done", "dropped"}

// OpenStatuses are the statuses that still need work.
var OpenStatuses = []string{"planned", "active", "blocked", "deferred"}

// LinkTypes and LinkRoles are the fixed link vocabularies.
var LinkTypes = []string{"job", "pr", "route", "note", "item", "url"}
var LinkRoles = []string{"delivers", "proof", "blocks", "related"}

// Milestone is a named target such as "v1".
type Milestone struct {
	Key         string `json:"key"`
	ProjectID   string `json:"project_id"`
	Revision    int64  `json:"revision"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	TargetDate  string `json:"target_date"`
	Position    int64  `json:"position"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// MilestoneFields is the editable part of a milestone.
type MilestoneFields struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	TargetDate  string `json:"target_date"`
	Position    int64  `json:"position"`
}

// Link is a typed edge from an item to another record or URL.
type Link struct {
	Type  string `json:"type"`
	Ref   string `json:"ref"`
	Role  string `json:"role"`
	Label string `json:"label"`
}

// Item is one trackable unit of work.
type Item struct {
	Key         string   `json:"key"`
	ProjectID   string   `json:"project_id"`
	Revision    int64    `json:"revision"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Milestone   string   `json:"milestone"`
	Parent      string   `json:"parent"`
	Labels      []string `json:"labels"`
	RevisitWhen string   `json:"revisit_when"`
	Position    int64    `json:"position"`
	Links       []Link   `json:"links"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// ItemFields is the editable part of an item, links included.
type ItemFields struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Milestone   string   `json:"milestone"`
	Parent      string   `json:"parent"`
	Labels      []string `json:"labels"`
	RevisitWhen string   `json:"revisit_when"`
	Position    int64    `json:"position"`
	Links       []Link   `json:"links"`
}

// Counts are item totals by status, computed on read.
type Counts struct {
	Planned  int64 `json:"planned"`
	Active   int64 `json:"active"`
	Blocked  int64 `json:"blocked"`
	Deferred int64 `json:"deferred"`
	Done     int64 `json:"done"`
	Dropped  int64 `json:"dropped"`
	Total    int64 `json:"total"`
}

// Add counts n items with status.
func (c *Counts) Add(status string, n int64) {
	switch status {
	case "planned":
		c.Planned += n
	case "active":
		c.Active += n
	case "blocked":
		c.Blocked += n
	case "deferred":
		c.Deferred += n
	case "done":
		c.Done += n
	case "dropped":
		c.Dropped += n
	}
	c.Total += n
}

// ItemSummary is the compact list row.
type ItemSummary struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Milestone string   `json:"milestone"`
	Parent    string   `json:"parent"`
	Labels    []string `json:"labels"`
	Revision  int64    `json:"revision"`
}

// ItemView is a full item plus the status counts of its direct children.
type ItemView struct {
	Item     Item   `json:"item"`
	Children Counts `json:"children"`
}

// MilestoneSummary is the compact list row with its item counts.
type MilestoneSummary struct {
	Key        string `json:"key"`
	Title      string `json:"title"`
	State      string `json:"state"`
	TargetDate string `json:"target_date"`
	Revision   int64  `json:"revision"`
	Counts     Counts `json:"counts"`
}

// MilestoneView is a full milestone with counts and a bounded page of open items.
type MilestoneView struct {
	Milestone     Milestone     `json:"milestone"`
	Counts        Counts        `json:"counts"`
	OpenItems     []ItemSummary `json:"open_items"`
	OpenItemsMore bool          `json:"open_items_more"`
}

// HistorySummary is one revision row without its captured content.
type HistorySummary struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
}

// ItemRevision is one captured item revision.
type ItemRevision struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
	State     Item   `json:"state"`
}

// MilestoneRevision is one captured milestone revision.
type MilestoneRevision struct {
	Revision  int64     `json:"revision"`
	ActorID   string    `json:"actor_id"`
	CreatedAt string    `json:"created_at"`
	State     Milestone `json:"state"`
}

// Receipt reports one keyed write.
type Receipt struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

// BatchReceipt reports every record of one batch, in request order.
type BatchReceipt struct {
	Results []Receipt `json:"results"`
}

// Page is one keyset page.
type Page[T any] struct {
	Items   []T    `json:"items"`
	Cursor  string `json:"cursor,omitempty"`
	HasMore bool   `json:"has_more"`
}

// LinkTarget names links to remove. An empty role removes every role.
type LinkTarget struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
	Role string `json:"role"`
}
