package presentation

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxStateBytes = presentationmodel.MaxConfigBytes + 2048

func invalid() error {
	return core.Fail("validation_failed", "Use a bounded project presentation with valid fields and revision.", 400)
}
func unavailable() error {
	return core.Fail("storage_unavailable", "Stored project presentation is unavailable.", 503)
}
func notFound() error {
	return core.Fail("not_found", "The selected presentation revision is unavailable.", 404)
}
func conflict(rev int64) error {
	return &core.Error{Code: "project_presentation_revision_conflict", Message: "Reload the current presentation before replacing it.", Status: 409, Details: map[string]int64{"current_revision": rev}}
}
func validID(s string) bool {
	return s != "" && s != "." && s != ".." && len(s) <= 128 && utf8.ValidString(s) && !strings.ContainsAny(s, "/\\%?#") && strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) < 0
}
func validStamp(v string) bool {
	t, e := time.Parse(time.RFC3339Nano, v)
	return e == nil && !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 && strings.HasSuffix(v, "Z")
}
func equalConfig(a, b Config) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func decodeState(raw string) (State, error) {
	var s State
	if len(raw) > maxStateBytes || strictjson.Decode([]byte(raw), &s) != nil || !validID(s.ProjectID) || s.Revision < 1 || !validStamp(s.UpdatedAt) || s.Headings == nil || s.Theme == nil {
		return State{}, unavailable()
	}
	c, err := presentationmodel.Normalize(s.Config())
	if err != nil || !equalConfig(c, s.Config()) {
		return State{}, unavailable()
	}
	encoded, err := json.Marshal(s)
	if err != nil || string(encoded) != raw {
		return State{}, unavailable()
	}
	return s, nil
}
