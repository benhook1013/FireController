package tracking

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// Register mounts provider-free milestone and item endpoints.
func Register(mux *http.ServeMux, store *core.Store) {
	base := "/api/v1/projects/{project_id}/tracking"
	mux.HandleFunc("POST "+base+"/batch", batch(store))
	mux.HandleFunc("GET "+base+"/items", listItems(store))
	mux.HandleFunc("GET "+base+"/items/{key}", getItem(store))
	mux.HandleFunc("PUT "+base+"/items/{key}", putItem(store))
	mux.HandleFunc("PATCH "+base+"/items/{key}", patchItem(store))
	mux.HandleFunc("POST "+base+"/items/{key}/links", changeLinks(store))
	mux.HandleFunc("GET "+base+"/items/{key}/history", itemHistory(store))
	mux.HandleFunc("GET "+base+"/items/{key}/history/{revision}", itemRevision(store))
	mux.HandleFunc("GET "+base+"/milestones", listMilestones(store))
	mux.HandleFunc("GET "+base+"/milestones/{key}", getMilestone(store))
	mux.HandleFunc("PUT "+base+"/milestones/{key}", putMilestone(store))
	mux.HandleFunc("PATCH "+base+"/milestones/{key}", patchMilestone(store))
	mux.HandleFunc("GET "+base+"/milestones/{key}/history", milestoneHistory(store))
	mux.HandleFunc("GET "+base+"/milestones/{key}/history/{revision}", milestoneRevision(store))
}

func scopeFor(store *core.Store, w http.ResponseWriter, r *http.Request, permission string, keys ...string) (core.Scope, bool) {
	scope, err := store.Scope(r, permission)
	if err == nil {
		_, err = core.QueryKeys(r, "invalid_query", keys...)
	}
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, false
	}
	return scope, true
}
func respond(w http.ResponseWriter, value any, err error, status int) {
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, status, value)
}
func pathKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("key")
	if !validKey(key) {
		core.WriteError(w, invalid("Keys are 1-64 characters of letters, digits and ._:- starting with a letter or digit."))
		return "", false
	}
	return key, true
}
func pageLimit(r *http.Request) (int, error) {
	if _, ok := r.URL.Query()["limit"]; !ok {
		return 25, nil
	}
	return core.PageLimit(r)
}

// readBody decodes one strict JSON object. Explicit null is refused anywhere:
// omission is the only way to leave a field unchanged or defaulted.
func readBody(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return invalid(fmt.Sprintf("The request body is unavailable or exceeds %d KiB.", limit>>10))
	}
	if strictjson.Validate(data) != nil {
		return invalid("Use one strict JSON object with unique keys and valid Unicode.")
	}
	if err = rejectNulls(data); err != nil {
		return err
	}
	if strictjson.Decode(data, dst) != nil {
		return invalid("Use one strict JSON object with declared fields.")
	}
	return nil
}

type itemInput struct {
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Status      *string   `json:"status"`
	Milestone   *string   `json:"milestone"`
	Parent      *string   `json:"parent"`
	Labels      *[]string `json:"labels"`
	RevisitWhen *string   `json:"revisit_when"`
	Position    *int64    `json:"position"`
	Links       *[]Link   `json:"links"`
}

func value[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// fields converts a complete item declaration. It reports whether links were
// omitted, in which case the current links are kept.
func (in itemInput) fields() (ItemFields, bool, error) {
	if in.Title == nil {
		return ItemFields{}, false, invalid("An item declaration needs a title.")
	}
	f := ItemFields{Title: *in.Title, Description: value(in.Description), Status: value(in.Status), Milestone: value(in.Milestone), Parent: value(in.Parent), Labels: value(in.Labels), RevisitWhen: value(in.RevisitWhen), Position: value(in.Position)}
	if in.Links != nil {
		f.Links = *in.Links
	}
	normalized, err := normalizeItem(f)
	if err != nil {
		return ItemFields{}, false, err
	}
	if in.Links == nil {
		normalized.Links = nil
	}
	return normalized, in.Links == nil, nil
}

type itemDeclaration struct {
	ExpectedRevision *int64     `json:"expected_revision"`
	Item             *itemInput `json:"item"`
}
type normalizedItem struct {
	Key              string     `json:"key"`
	ExpectedRevision *int64     `json:"expected_revision,omitempty"`
	Item             ItemFields `json:"item"`
	KeepLinks        bool       `json:"keep_links"`
}

func (d itemDeclaration) normalize(key string) (normalizedItem, error) {
	if d.Item == nil {
		return normalizedItem{}, invalid("Supply an item object.")
	}
	if d.ExpectedRevision != nil && *d.ExpectedRevision < 0 {
		return normalizedItem{}, invalid("expected_revision is 0 (create only) or a current revision.")
	}
	f, keep, err := d.Item.fields()
	if err != nil {
		return normalizedItem{}, err
	}
	return normalizedItem{key, d.ExpectedRevision, f, keep}, nil
}

type milestoneInput struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	State       *string `json:"state"`
	TargetDate  *string `json:"target_date"`
	Position    *int64  `json:"position"`
}
type milestoneDeclaration struct {
	ExpectedRevision *int64          `json:"expected_revision"`
	Milestone        *milestoneInput `json:"milestone"`
}
type normalizedMilestone struct {
	Key              string          `json:"key"`
	ExpectedRevision *int64          `json:"expected_revision,omitempty"`
	Milestone        MilestoneFields `json:"milestone"`
}

func (d milestoneDeclaration) normalize(key string) (normalizedMilestone, error) {
	if d.Milestone == nil || d.Milestone.Title == nil {
		return normalizedMilestone{}, invalid("A milestone declaration needs a title.")
	}
	if d.ExpectedRevision != nil && *d.ExpectedRevision < 0 {
		return normalizedMilestone{}, invalid("expected_revision is 0 (create only) or a current revision.")
	}
	in := d.Milestone
	f, err := normalizeMilestone(MilestoneFields{Title: *in.Title, Description: value(in.Description), State: value(in.State), TargetDate: value(in.TargetDate), Position: value(in.Position)})
	return normalizedMilestone{key, d.ExpectedRevision, f}, err
}

func mutate(store *core.Store, w http.ResponseWriter, r *http.Request, scope core.Scope, operation, target string, request any, fn func(context.Context, *sql.Tx) (any, error)) {
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: operation, Target: target, Key: key, Request: request}, fn)
	respond(w, json.RawMessage(result), err, http.StatusOK)
}

func putItem(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		var d itemDeclaration
		if err := readBody(w, r, &d, 64<<10); err != nil {
			core.WriteError(w, err)
			return
		}
		n, err := d.normalize(key)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		mutate(store, w, r, scope, "tracking.items.put", key, n, func(ctx context.Context, tx *sql.Tx) (any, error) {
			repository, err := projectRepository(ctx, tx, scope.ProjectID)
			if err != nil {
				return nil, err
			}
			return applyItem(ctx, tx, scope, repository, key, n.ExpectedRevision, n.Item, n.KeepLinks)
		})
	}
}

type itemChanges struct {
	Title        *string   `json:"title"`
	Description  *string   `json:"description"`
	Status       *string   `json:"status"`
	Milestone    *string   `json:"milestone"`
	Parent       *string   `json:"parent"`
	Labels       *[]string `json:"labels"`
	AddLabels    []string  `json:"add_labels"`
	RemoveLabels []string  `json:"remove_labels"`
	RevisitWhen  *string   `json:"revisit_when"`
	Position     *int64    `json:"position"`
}
type itemPatch struct {
	ExpectedRevision *int64       `json:"expected_revision"`
	Changes          *itemChanges `json:"changes"`
}

func (c itemChanges) empty() bool {
	return c.Title == nil && c.Description == nil && c.Status == nil && c.Milestone == nil && c.Parent == nil && c.Labels == nil && len(c.AddLabels) == 0 && len(c.RemoveLabels) == 0 && c.RevisitWhen == nil && c.Position == nil
}

func (c itemChanges) apply(f ItemFields) (ItemFields, error) {
	if c.Title != nil {
		f.Title = *c.Title
	}
	if c.Description != nil {
		f.Description = *c.Description
	}
	if c.Status != nil {
		f.Status = *c.Status
	}
	if c.Milestone != nil {
		f.Milestone = *c.Milestone
	}
	if c.Parent != nil {
		f.Parent = *c.Parent
	}
	if c.Labels != nil {
		f.Labels = slices.Clone(*c.Labels)
	}
	f.Labels = append(f.Labels, c.AddLabels...)
	f.Labels = slices.DeleteFunc(f.Labels, func(l string) bool { return slices.Contains(c.RemoveLabels, l) })
	if c.RevisitWhen != nil {
		f.RevisitWhen = *c.RevisitWhen
	}
	if c.Position != nil {
		f.Position = *c.Position
	}
	return normalizeItem(f)
}

func patchItem(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		var p itemPatch
		if err := readBody(w, r, &p, 64<<10); err != nil {
			core.WriteError(w, err)
			return
		}
		if p.Changes == nil || p.Changes.empty() {
			core.WriteError(w, invalid("Supply at least one change."))
			return
		}
		if (p.Changes.Title != nil || p.Changes.Description != nil) && (p.ExpectedRevision == nil || *p.ExpectedRevision < 1) {
			core.WriteError(w, invalid("Title and description changes need expected_revision."))
			return
		}
		if p.ExpectedRevision != nil && *p.ExpectedRevision < 1 {
			core.WriteError(w, invalid("expected_revision on a change is a current revision of at least 1."))
			return
		}
		for _, l := range append(append([]string{}, p.Changes.AddLabels...), p.Changes.RemoveLabels...) {
			if !labelPattern.MatchString(l) {
				core.WriteError(w, invalid("Labels are 1-64 characters of letters, digits and ._:/+- starting with a letter or digit."))
				return
			}
		}
		mutate(store, w, r, scope, "tracking.items.patch", key, p, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, exists, err := readItem(ctx, tx, scope.ProjectID, key)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, notFound()
			}
			f, err := p.Changes.apply(itemFields(current))
			if err != nil {
				return nil, err
			}
			repository, err := projectRepository(ctx, tx, scope.ProjectID)
			if err != nil {
				return nil, err
			}
			return applyItem(ctx, tx, scope, repository, key, p.ExpectedRevision, f, false)
		})
	}
}

type linkChange struct {
	ExpectedRevision *int64       `json:"expected_revision"`
	Add              []Link       `json:"add"`
	Remove           []LinkTarget `json:"remove"`
}

// applyLinks adds links (replacing the label of an existing identical link)
// and removes matching links. It is an idempotent set operation.
func (c linkChange) apply(links []Link, repository string) ([]Link, error) {
	out := slices.Clone(links)
	for _, t := range c.Remove {
		if t.Role != "" && !slices.Contains(LinkRoles, t.Role) || !slices.Contains(LinkTypes, t.Type) {
			return nil, invalid("Remove targets need a valid type, ref and optional role.")
		}
		ref, err := normalizeRef(t.Type, t.Ref, repository)
		if err != nil {
			return nil, err
		}
		out = slices.DeleteFunc(out, func(l Link) bool { return l.Type == t.Type && l.Ref == ref && (t.Role == "" || l.Role == t.Role) })
	}
	for _, a := range c.Add {
		n, err := normalizeLinkShape(a, repository)
		if err != nil {
			return nil, err
		}
		out = slices.DeleteFunc(out, func(l Link) bool { return l.Type == n.Type && l.Ref == n.Ref && l.Role == n.Role })
		out = append(out, n)
	}
	return out, nil
}

func changeLinks(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		var c linkChange
		if err := readBody(w, r, &c, 64<<10); err != nil {
			core.WriteError(w, err)
			return
		}
		if len(c.Add)+len(c.Remove) == 0 || len(c.Add) > maxLinks || len(c.Remove) > maxLinks {
			core.WriteError(w, invalid("Supply 1-64 links to add and/or remove."))
			return
		}
		if c.ExpectedRevision != nil && *c.ExpectedRevision < 1 {
			core.WriteError(w, invalid("expected_revision on a change is a current revision of at least 1."))
			return
		}
		mutate(store, w, r, scope, "tracking.items.links", key, c, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, exists, err := readItem(ctx, tx, scope.ProjectID, key)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, notFound()
			}
			repository, err := projectRepository(ctx, tx, scope.ProjectID)
			if err != nil {
				return nil, err
			}
			f := itemFields(current)
			if f.Links, err = c.apply(f.Links, repository); err != nil {
				return nil, err
			}
			return applyItem(ctx, tx, scope, repository, key, c.ExpectedRevision, f, false)
		})
	}
}

func putMilestone(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		var d milestoneDeclaration
		if err := readBody(w, r, &d, 64<<10); err != nil {
			core.WriteError(w, err)
			return
		}
		n, err := d.normalize(key)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		mutate(store, w, r, scope, "tracking.milestones.put", key, n, func(ctx context.Context, tx *sql.Tx) (any, error) {
			return applyMilestone(ctx, tx, scope, key, n.ExpectedRevision, n.Milestone)
		})
	}
}

type milestonePatch struct {
	ExpectedRevision *int64          `json:"expected_revision"`
	Changes          *milestoneInput `json:"changes"`
}

func patchMilestone(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		var p milestonePatch
		if err := readBody(w, r, &p, 64<<10); err != nil {
			core.WriteError(w, err)
			return
		}
		c := p.Changes
		if c == nil || c.Title == nil && c.Description == nil && c.State == nil && c.TargetDate == nil && c.Position == nil {
			core.WriteError(w, invalid("Supply at least one change."))
			return
		}
		if (c.Title != nil || c.Description != nil) && (p.ExpectedRevision == nil || *p.ExpectedRevision < 1) {
			core.WriteError(w, invalid("Title and description changes need expected_revision."))
			return
		}
		if p.ExpectedRevision != nil && *p.ExpectedRevision < 1 {
			core.WriteError(w, invalid("expected_revision on a change is a current revision of at least 1."))
			return
		}
		mutate(store, w, r, scope, "tracking.milestones.patch", key, p, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, exists, err := readMilestone(ctx, tx, scope.ProjectID, key)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, notFound()
			}
			f := milestoneFields(current)
			if c.Title != nil {
				f.Title = *c.Title
			}
			if c.Description != nil {
				f.Description = *c.Description
			}
			if c.State != nil {
				f.State = *c.State
			}
			if c.TargetDate != nil {
				f.TargetDate = *c.TargetDate
			}
			if c.Position != nil {
				f.Position = *c.Position
			}
			if f.State == "" {
				return nil, invalid("State is open or closed.")
			}
			return applyMilestone(ctx, tx, scope, key, p.ExpectedRevision, f)
		})
	}
}

type batchItem struct {
	Key              string     `json:"key"`
	ExpectedRevision *int64     `json:"expected_revision"`
	Item             *itemInput `json:"item"`
}
type batchMilestone struct {
	Key              string          `json:"key"`
	ExpectedRevision *int64          `json:"expected_revision"`
	Milestone        *milestoneInput `json:"milestone"`
}
type batchRequest struct {
	Milestones []batchMilestone `json:"milestones"`
	Items      []batchItem      `json:"items"`
}

// batch declares milestones then items, in order, in one transaction. Each
// record follows the single-record PUT rules, so reapplying a file is a no-op.
func batch(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		var b batchRequest
		if err := readBody(w, r, &b, 2<<20); err != nil {
			core.WriteError(w, err)
			return
		}
		if len(b.Items)+len(b.Milestones) == 0 || len(b.Items) > maxBatchItems || len(b.Milestones) > maxBatchMilestones {
			core.WriteError(w, invalid("A batch holds 1-100 items and up to 20 milestones."))
			return
		}
		milestones := make([]normalizedMilestone, 0, len(b.Milestones))
		seen := map[string]bool{}
		for _, m := range b.Milestones {
			if !validKey(m.Key) || seen["m:"+m.Key] {
				core.WriteError(w, invalid("Each batch milestone needs a unique valid key."))
				return
			}
			seen["m:"+m.Key] = true
			n, err := milestoneDeclaration{m.ExpectedRevision, m.Milestone}.normalize(m.Key)
			if err != nil {
				core.WriteError(w, err)
				return
			}
			milestones = append(milestones, n)
		}
		items := make([]normalizedItem, 0, len(b.Items))
		for _, i := range b.Items {
			if !validKey(i.Key) || seen["i:"+i.Key] {
				core.WriteError(w, invalid("Each batch item needs a unique valid key."))
				return
			}
			seen["i:"+i.Key] = true
			n, err := itemDeclaration{i.ExpectedRevision, i.Item}.normalize(i.Key)
			if err != nil {
				core.WriteError(w, err)
				return
			}
			items = append(items, n)
		}
		request := struct {
			Milestones []normalizedMilestone `json:"milestones"`
			Items      []normalizedItem      `json:"items"`
		}{milestones, items}
		mutate(store, w, r, scope, "tracking.batch", "tracking", request, func(ctx context.Context, tx *sql.Tx) (any, error) {
			repository, err := projectRepository(ctx, tx, scope.ProjectID)
			if err != nil {
				return nil, err
			}
			out := BatchReceipt{Results: make([]Receipt, 0, len(milestones)+len(items))}
			for _, m := range milestones {
				receipt, err := applyMilestone(ctx, tx, scope, m.Key, m.ExpectedRevision, m.Milestone)
				if err != nil {
					return nil, batchError(err, "milestone", m.Key)
				}
				out.Results = append(out.Results, receipt)
			}
			for _, i := range items {
				receipt, err := applyItem(ctx, tx, scope, repository, i.Key, i.ExpectedRevision, i.Item, i.KeepLinks)
				if err != nil {
					return nil, batchError(err, "item", i.Key)
				}
				out.Results = append(out.Results, receipt)
			}
			return out, nil
		})
	}
}

// batchError names the failing record so a large declaration is easy to fix.
func batchError(err error, kind, key string) error {
	if e, ok := err.(*core.Error); ok && e.Status < 500 {
		copy := *e
		copy.Message = kind + " " + key + ": " + e.Message
		return &copy
	}
	return err
}

// itemQuery holds the validated list filters.
type itemQuery struct {
	milestone, parent, label, linkType, linkRef string
	statuses                                    []string
}

func parseItemQuery(r *http.Request) (itemQuery, error) {
	q := r.URL.Query()
	out := itemQuery{milestone: q.Get("milestone"), parent: q.Get("parent"), label: q.Get("label")}
	if out.milestone != "" && !validKey(out.milestone) || out.parent != "" && !validKey(out.parent) || out.label != "" && !labelPattern.MatchString(out.label) {
		return out, invalid("Filter by valid milestone, parent and label keys.")
	}
	if v := q.Get("status"); v != "" {
		for _, s := range strings.Split(v, ",") {
			if !validStatus(s) || slices.Contains(out.statuses, s) {
				return out, invalid("status is a comma-separated list of planned, active, blocked, deferred, done or dropped.")
			}
			out.statuses = append(out.statuses, s)
		}
	}
	if v := q.Get("link"); v != "" {
		kind, ref, ok := strings.Cut(v, ":")
		if !ok || !slices.Contains(LinkTypes, kind) || ref == "" {
			return out, invalid("link is TYPE:REF, for example pr:42 or job:job_1.")
		}
		out.linkType, out.linkRef = kind, ref
	}
	return out, nil
}

func (q itemQuery) filter() string {
	return strings.Join([]string{"tracking.items", q.milestone, q.parent, q.label, strings.Join(q.statuses, ","), q.linkType, q.linkRef}, "\x1f")
}

func parseOrderCursor(store *core.Store, scope core.Scope, filter, cursor string) (int64, string, error) {
	value, err := store.ParseCursor(scope, filter, cursor)
	if err != nil || value == "" {
		return -1, "", err
	}
	pos, key, ok := strings.Cut(value, "/")
	n, perr := strconv.ParseInt(pos, 10, 64)
	if !ok || perr != nil || n < 0 || !validKey(key) {
		return 0, "", invalid("Use the cursor returned by the previous page.")
	}
	return n, key, nil
}

func scanSummaries(rows *sql.Rows, project string, limit int) ([]ItemSummary, bool, int64, error) {
	defer rows.Close()
	out := make([]ItemSummary, 0, limit)
	var lastPosition int64
	more := false
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return nil, false, 0, err
		}
		if !raw.Valid {
			return nil, false, 0, unavailable()
		}
		item, err := decodeItem(raw.String)
		if err != nil {
			return nil, false, 0, err
		}
		if item.ProjectID != project {
			return nil, false, 0, unavailable()
		}
		if len(out) == limit {
			more = true
			break
		}
		lastPosition = item.Position
		out = append(out, ItemSummary{Key: item.Key, Title: item.Title, Status: item.Status, Milestone: item.Milestone, Parent: item.Parent, Labels: item.Labels, Revision: item.Revision})
	}
	return out, more, lastPosition, rows.Err()
}

const recordColumn = `CASE WHEN length(CAST(record_json AS BLOB))<=` + "32768" + ` THEN record_json ELSE NULL END`

func listItems(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read", "milestone", "parent", "status", "label", "link", "limit", "cursor")
		if !ok {
			return
		}
		q, err := parseItemQuery(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		limit, err := pageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		afterPos, afterKey, err := parseOrderCursor(store, scope, q.filter(), r.URL.Query().Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := Page[ItemSummary]{}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			query := `SELECT ` + recordColumn + ` FROM tracking_items WHERE project_id=?`
			args := []any{scope.ProjectID}
			if q.milestone != "" {
				query += ` AND milestone_key=?`
				args = append(args, q.milestone)
			}
			if q.parent != "" {
				query += ` AND parent_key=?`
				args = append(args, q.parent)
			}
			if len(q.statuses) > 0 {
				query += ` AND status IN (?` + strings.Repeat(",?", len(q.statuses)-1) + `)`
				for _, s := range q.statuses {
					args = append(args, s)
				}
			}
			if q.label != "" {
				query += ` AND EXISTS(SELECT 1 FROM json_each(record_json,'$.labels') WHERE value=?)`
				args = append(args, q.label)
			}
			if q.linkType != "" {
				repository, err := projectRepository(ctx, tx, scope.ProjectID)
				if err != nil {
					return err
				}
				ref, err := normalizeRef(q.linkType, q.linkRef, repository)
				if err != nil {
					return err
				}
				query += ` AND EXISTS(SELECT 1 FROM tracking_item_links l WHERE l.project_id=tracking_items.project_id AND l.item_key=tracking_items.key AND l.target_type=? AND l.target_ref=?)`
				args = append(args, q.linkType, ref)
			}
			if afterPos >= 0 {
				query += ` AND (position>? OR position=? AND key>?)`
				args = append(args, afterPos, afterPos, afterKey)
			}
			query += ` ORDER BY position,key LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			var last int64
			result.Items, result.HasMore, last, err = scanSummaries(rows, scope.ProjectID, limit)
			if err == nil && result.HasMore {
				result.Cursor = store.Cursor(scope, q.filter(), strconv.FormatInt(last, 10)+"/"+result.Items[len(result.Items)-1].Key)
			}
			return err
		})
		respond(w, result, err, http.StatusOK)
	}
}

func getItem(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		var view ItemView
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			item, exists, err := readItem(ctx, tx, scope.ProjectID, key)
			if err != nil {
				return err
			}
			if !exists {
				return notFound()
			}
			view.Item = item
			view.Children, err = countBy(ctx, tx, `SELECT status,count(*) FROM tracking_items WHERE project_id=? AND parent_key=? GROUP BY status`, scope.ProjectID, key)
			return err
		})
		respond(w, view, err, http.StatusOK)
	}
}

func listMilestones(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read", "state")
		if !ok {
			return
		}
		state := r.URL.Query().Get("state")
		if state != "" && state != "open" && state != "closed" {
			core.WriteError(w, invalid("state is open or closed."))
			return
		}
		result := Page[MilestoneSummary]{Items: []MilestoneSummary{}}
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			query := `SELECT key FROM tracking_milestones WHERE project_id=?`
			args := []any{scope.ProjectID}
			if state != "" {
				query += ` AND state=?`
				args = append(args, state)
			}
			// Milestones are few by design; the bounded list needs no cursor.
			rows, err := tx.QueryContext(ctx, query+` ORDER BY COALESCE(json_extract(record_json,'$.position'),0),key LIMIT 201`, args...)
			if err != nil {
				return err
			}
			var keys []string
			for rows.Next() {
				var k string
				if err = rows.Scan(&k); err != nil {
					rows.Close()
					return err
				}
				keys = append(keys, k)
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				return err
			}
			positions := map[string]int64{}
			if len(keys) > 200 {
				keys, result.HasMore = keys[:200], true
			}
			for _, k := range keys {
				m, exists, err := readMilestone(ctx, tx, scope.ProjectID, k)
				if err != nil || !exists {
					return unavailable()
				}
				counts, err := countBy(ctx, tx, `SELECT status,count(*) FROM tracking_items WHERE project_id=? AND milestone_key=? GROUP BY status`, scope.ProjectID, k)
				if err != nil {
					return err
				}
				positions[k] = m.Position
				result.Items = append(result.Items, MilestoneSummary{Key: m.Key, Title: m.Title, State: m.State, TargetDate: m.TargetDate, Revision: m.Revision, Counts: counts})
			}
			slices.SortStableFunc(result.Items, func(a, b MilestoneSummary) int { return int(positions[a.Key] - positions[b.Key]) })
			return nil
		})
		respond(w, result, err, http.StatusOK)
	}
}

func getMilestone(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		var view MilestoneView
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			m, exists, err := readMilestone(ctx, tx, scope.ProjectID, key)
			if err != nil {
				return err
			}
			if !exists {
				return notFound()
			}
			view.Milestone = m
			if view.Counts, err = countBy(ctx, tx, `SELECT status,count(*) FROM tracking_items WHERE project_id=? AND milestone_key=? GROUP BY status`, scope.ProjectID, key); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT `+recordColumn+` FROM tracking_items WHERE project_id=? AND milestone_key=? AND status IN ('planned','active','blocked','deferred') ORDER BY position,key LIMIT 101`, scope.ProjectID, key)
			if err != nil {
				return err
			}
			view.OpenItems, view.OpenItemsMore, _, err = scanSummaries(rows, scope.ProjectID, 100)
			return err
		})
		respond(w, view, err, http.StatusOK)
	}
}

func parseRevision(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("revision")
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != raw {
		core.WriteError(w, invalid("Select a revision of at least 1."))
		return 0, false
	}
	return n, true
}

// history serves a newest-first bounded page of revision summaries for one
// record in table (keyed by column), after checking the record exists.
func history(store *core.Store, table, column, current string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read", "limit", "cursor")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		limit, err := pageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := table + ":" + key
		value, err := store.ParseCursor(scope, filter, r.URL.Query().Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var before int64
		if value != "" {
			if before, err = strconv.ParseInt(value, 10, 64); err != nil || before < 1 {
				core.WriteError(w, invalid("Use the cursor returned by the previous page."))
				return
			}
		}
		result := Page[HistorySummary]{Items: make([]HistorySummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+current+` WHERE project_id=? AND key=?`, scope.ProjectID, key).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return notFound()
			}
			query := `SELECT revision,actor_id,created_at FROM ` + table + ` WHERE project_id=? AND ` + column + `=?`
			args := []any{scope.ProjectID, key}
			if before > 0 {
				query += ` AND revision<?`
				args = append(args, before)
			}
			rows, err := tx.QueryContext(ctx, query+` ORDER BY revision DESC LIMIT ?`, append(args, limit+1)...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var h HistorySummary
				if err = rows.Scan(&h.Revision, &h.ActorID, &h.CreatedAt); err != nil {
					return err
				}
				if len(result.Items) == limit {
					result.HasMore = true
					break
				}
				result.Items = append(result.Items, h)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if result.HasMore {
				result.Cursor = store.Cursor(scope, filter, strconv.FormatInt(result.Items[len(result.Items)-1].Revision, 10))
			}
			return nil
		})
		respond(w, result, err, http.StatusOK)
	}
}

func itemHistory(store *core.Store) http.HandlerFunc {
	return history(store, "tracking_item_history", "item_key", "tracking_items")
}
func milestoneHistory(store *core.Store) http.HandlerFunc {
	return history(store, "tracking_milestone_history", "milestone_key", "tracking_milestones")
}

func readHistoryRow(ctx context.Context, tx *sql.Tx, table, column, project, key string, rev int64) (string, string, string, error) {
	var actor, created string
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT actor_id,created_at,`+recordColumn+` FROM `+table+` WHERE project_id=? AND `+column+`=? AND revision=?`, project, key, rev).Scan(&actor, &created, &raw)
	if err == sql.ErrNoRows {
		return "", "", "", notFound()
	}
	if err != nil {
		return "", "", "", err
	}
	if !raw.Valid {
		return "", "", "", unavailable()
	}
	return actor, created, raw.String, nil
}

func itemRevision(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		rev, ok := parseRevision(w, r)
		if !ok {
			return
		}
		var out ItemRevision
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			actor, created, raw, err := readHistoryRow(ctx, tx, "tracking_item_history", "item_key", scope.ProjectID, key, rev)
			if err != nil {
				return err
			}
			item, err := decodeItem(raw)
			if err != nil {
				return err
			}
			if item.Key != key || item.ProjectID != scope.ProjectID || item.Revision != rev || item.UpdatedAt != created {
				return unavailable()
			}
			out = ItemRevision{Revision: rev, ActorID: actor, CreatedAt: created, State: item}
			return nil
		})
		respond(w, out, err, http.StatusOK)
	}
}

func milestoneRevision(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read")
		if !ok {
			return
		}
		key, ok := pathKey(w, r)
		if !ok {
			return
		}
		rev, ok := parseRevision(w, r)
		if !ok {
			return
		}
		var out MilestoneRevision
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			actor, created, raw, err := readHistoryRow(ctx, tx, "tracking_milestone_history", "milestone_key", scope.ProjectID, key, rev)
			if err != nil {
				return err
			}
			m, err := decodeMilestone(raw)
			if err != nil {
				return err
			}
			if m.Key != key || m.ProjectID != scope.ProjectID || m.Revision != rev || m.UpdatedAt != created {
				return unavailable()
			}
			out = MilestoneRevision{Revision: rev, ActorID: actor, CreatedAt: created, State: m}
			return nil
		})
		respond(w, out, err, http.StatusOK)
	}
}
