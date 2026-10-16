package projectmap

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxEditableBytes = 64 << 10
const maxEntryBytes = maxEditableBytes + 2048

var phaseID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var entryID = regexp.MustCompile(`^map_entry_[0-9a-f]{32}$`)

func invalid() error {
	return core.Fail("validation_failed", "Use a complete bounded project-map value with valid fields.", 400)
}
func unavailable() error {
	return core.Fail("storage_unavailable", "Stored project-map state is unavailable.", 503)
}
func notFound() error {
	return core.Fail("not_found", "The selected record is unavailable in this project.", 404)
}
func conflict(rev int64) error {
	return &core.Error{Code: "revision_conflict", Message: "Reload the current record before replacing it.", Status: 409, Details: map[string]int64{"current_revision": rev}}
}
func validID(v string) bool {
	return v != "" && len(v) <= 128 && utf8.ValidString(v) && v != "." && v != ".." && !strings.ContainsAny(v, "/\\%?#") && strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) < 0
}
func text(v string, n int) bool {
	return len(v) <= n && utf8.ValidString(v) && strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) < 0
}
func short(v string, n int) bool { return text(v, n) && strings.IndexFunc(v, unicode.IsControl) < 0 }
func named(v string, n int) bool { return short(v, n) && v != "" && strings.TrimSpace(v) == v }
func stamp() string              { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }
func validStamp(v string) bool {
	t, e := time.Parse(time.RFC3339Nano, v)
	return e == nil && !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 && strings.HasSuffix(v, "Z")
}
func editable(e Entry) Editable {
	return Editable{Kind: e.Kind, Title: e.Title, Description: e.Description, State: e.State, Where: e.Where, Next: e.Next, Trigger: e.Trigger, Phases: e.Phases, Links: e.Links, Archived: e.Archived}
}
func entry(e Editable, project, id string, rev int64, created, updated string) Entry {
	return Entry{ID: id, ProjectID: project, Revision: rev, Kind: e.Kind, Title: e.Title, Description: e.Description, State: e.State, Where: e.Where, Next: e.Next, Trigger: e.Trigger, Phases: e.Phases, Links: e.Links, Archived: e.Archived, CreatedAt: created, UpdatedAt: updated}
}
func validateEditable(e Editable) error {
	if e.Kind != "workstream" && e.Kind != "return_point" || !named(e.Title, 240) || !short(e.State, 240) || !text(e.Description, 8<<10) || !text(e.Where, 8<<10) || !text(e.Next, 8<<10) || !text(e.Trigger, 8<<10) || e.Phases == nil || e.Links == nil || len(e.Phases) > 64 || len(e.Links) > 12 {
		return invalid()
	}
	if e.Kind == "workstream" && e.Trigger != "" || e.Kind == "return_point" && (e.Where != "" || e.Next != "" || len(e.Phases) != 0) {
		return invalid()
	}
	ids := make(map[string]bool, len(e.Phases))
	for _, p := range e.Phases {
		if !phaseID.MatchString(p.ID) || ids[p.ID] || !named(p.Name, 160) || !short(p.State, 240) {
			return invalid()
		}
		ids[p.ID] = true
	}
	for _, l := range e.Links {
		u, err := url.Parse(l.URL)
		if !named(l.Label, 120) || len(l.URL) > 2048 || !utf8.ValidString(l.URL) || strings.IndexFunc(l.URL, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 || err != nil || u == nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
			return invalid()
		}
	}
	data, err := json.Marshal(e)
	if err != nil || len(data) > maxEditableBytes {
		return invalid()
	}
	return nil
}
func retainsPhases(old, next Editable) bool {
	ids := make(map[string]bool, len(next.Phases))
	for _, p := range next.Phases {
		ids[p.ID] = true
	}
	for _, p := range old.Phases {
		if !ids[p.ID] {
			return false
		}
	}
	return true
}
func equalEditable(a, b Editable) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func validEntry(e Entry) bool {
	return entryID.MatchString(e.ID) && validID(e.ProjectID) && e.Revision > 0 && validStamp(e.CreatedAt) && validStamp(e.UpdatedAt) && validateEditable(editable(e)) == nil
}
func decodeEntry(raw string) (Entry, error) {
	var e Entry
	if len(raw) > maxEntryBytes || strictjson.Decode([]byte(raw), &e) != nil || !validEntry(e) {
		return Entry{}, unavailable()
	}
	canonical, err := json.Marshal(e)
	if err != nil || string(canonical) != raw {
		return Entry{}, unavailable()
	}
	return e, nil
}

type phaseRequest struct {
	ID       *string `json:"id"`
	Name     *string `json:"name"`
	State    *string `json:"state"`
	Archived *bool   `json:"archived"`
}
type linkRequest struct {
	Label *string `json:"label"`
	URL   *string `json:"url"`
}
type editableRequest struct {
	Kind        *string        `json:"kind"`
	Title       *string        `json:"title"`
	Description *string        `json:"description"`
	State       *string        `json:"state"`
	Where       *string        `json:"where"`
	Next        *string        `json:"next"`
	Trigger     *string        `json:"trigger"`
	Phases      []phaseRequest `json:"phases"`
	Links       []linkRequest  `json:"links"`
	Archived    *bool          `json:"archived"`
}

// Optional scalar omission supplies the replacement default. Explicit null is
// distinct and invalid; collections deliberately normalize null and omission.
func (r *editableRequest) UnmarshalJSON(data []byte) error {
	type plain editableRequest
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if err := nonnullScalars(data, "phases", "links"); err != nil {
		return err
	}
	*r = editableRequest(value)
	return nil
}
func (r *phaseRequest) UnmarshalJSON(data []byte) error {
	type plain phaseRequest
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if err := nonnullScalars(data); err != nil {
		return err
	}
	*r = phaseRequest(value)
	return nil
}
func nonnullScalars(data []byte, collections ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return invalid()
	}
	for key, value := range fields {
		allowed := false
		for _, c := range collections {
			if key == c {
				allowed = true
			}
		}
		if !allowed && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid()
		}
	}
	return nil
}
func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func boolValue(v *bool) bool { return v != nil && *v }
func (r editableRequest) value() (Editable, error) {
	if r.Kind == nil || r.Title == nil {
		return Editable{}, invalid()
	}
	e := Editable{Kind: *r.Kind, Title: strings.TrimSpace(*r.Title), Description: stringValue(r.Description), State: stringValue(r.State), Where: stringValue(r.Where), Next: stringValue(r.Next), Trigger: stringValue(r.Trigger), Archived: boolValue(r.Archived), Phases: make([]Phase, 0, len(r.Phases)), Links: make([]Link, 0, len(r.Links))}
	for _, p := range r.Phases {
		if p.ID == nil || p.Name == nil {
			return Editable{}, invalid()
		}
		e.Phases = append(e.Phases, Phase{*p.ID, strings.TrimSpace(*p.Name), stringValue(p.State), boolValue(p.Archived)})
	}
	for _, l := range r.Links {
		if l.Label == nil || l.URL == nil {
			return Editable{}, invalid()
		}
		e.Links = append(e.Links, Link{strings.TrimSpace(*l.Label), *l.URL})
	}
	return e, validateEditable(e)
}

type replaceRequest struct {
	ExpectedRevision *int64           `json:"expected_revision"`
	Entry            *editableRequest `json:"entry"`
}
type replacement struct {
	ExpectedRevision int64    `json:"expected_revision"`
	Entry            Editable `json:"entry"`
}

func validGuard(p *int64, min int64) bool { return p != nil && *p >= min && *p < math.MaxInt64 }
