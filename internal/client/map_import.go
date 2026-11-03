package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxMapImportDocument = 256 << 10

type MapImportSource struct {
	Key                   string `json:"key"`
	RepositoryID          int64  `json:"repository_id"`
	Kind                  string `json:"kind"`
	Snapshot              string `json:"snapshot"`
	CollectedAt           string `json:"collected_at"`
	TransformationVersion int64  `json:"transformation_version"`
	Scope                 string `json:"scope"`
}
type MapImportProvenance struct {
	References  []string `json:"references"`
	Inferred    bool     `json:"inferred"`
	Confidence  string   `json:"confidence"`
	Uncertainty string   `json:"uncertainty"`
}
type MapImportItem struct {
	SourceKey        string              `json:"source_key"`
	ExpectedRevision int64               `json:"expected_revision"`
	Entry            ProjectMapEditable  `json:"entry"`
	Provenance       MapImportProvenance `json:"provenance"`
}
type MapImportDocument struct {
	Version int64           `json:"version"`
	Source  MapImportSource `json:"source"`
	Entries []MapImportItem `json:"entries"`
}
type MapImportPlan struct {
	SourceKey       string `json:"source_key"`
	Action          string `json:"action"`
	EntryID         string `json:"entry_id"`
	CurrentRevision int64  `json:"current_revision"`
	Reason          string `json:"reason"`
}
type MapImportPreview struct {
	Document      MapImportDocument `json:"document"`
	Ready         bool              `json:"ready"`
	PreviewSHA256 string            `json:"preview_sha256"`
	Plan          []MapImportPlan   `json:"plan"`
	Conflicts     int64             `json:"conflicts"`
}
type MapImportReceipt struct {
	SourceKey string `json:"source_key"`
	Revision  int64  `json:"revision"`
	Outcome   string `json:"outcome"`
	Created   int64  `json:"created"`
	Updated   int64  `json:"updated"`
	Unchanged int64  `json:"unchanged"`
}
type MapImportMapping struct {
	SourceKey      string `json:"source_key"`
	EntryID        string `json:"entry_id"`
	EntryRevision  int64  `json:"entry_revision"`
	SourceRevision int64  `json:"source_revision"`
}
type MapImportRecord struct {
	SourceKey string             `json:"source_key"`
	Revision  int64              `json:"revision"`
	Document  MapImportDocument  `json:"document"`
	Mappings  []MapImportMapping `json:"mappings"`
	ActorID   string             `json:"actor_id"`
	WorkerID  string             `json:"worker_id"`
	CreatedAt string             `json:"created_at"`
}
type MapImportHistorySummary struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	WorkerID  string `json:"worker_id"`
	CreatedAt string `json:"created_at"`
}

func ValidMapImportSourceKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for i, c := range []byte(key) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-')) {
			return false
		}
	}
	return true
}
func ValidMapImportCursor(cursor string) bool { return validOpaqueCursor(cursor) }
func mapImportEntryID(id string) bool {
	return strings.HasPrefix(id, "map_entry_") && controlHex(strings.TrimPrefix(id, "map_entry_"), 32)
}
func mapImportValidation() error {
	return fail("validation_failed", "Provide one bounded strict prepared map import document.", "Correct the selected file before loading credentials or contacting the service.", 400)
}
func mapImportFields(raw []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}
func mapImportWrapper(hash string, d MapImportDocument) ([]byte, error) {
	return json.Marshal(struct {
		Document      MapImportDocument `json:"document"`
		PreviewSHA256 string            `json:"preview_sha256"`
	}{d, hash})
}

// DecodeMapImportDocument normalizes the supported prepared format locally. It
// grants no destination admission; that remains the service preview's job.
func DecodeMapImportDocument(raw []byte) (MapImportDocument, error) {
	var d MapImportDocument
	if len(raw) == 0 || len(raw) > maxMapImportDocument || strictjson.Validate(raw) != nil {
		return d, mapImportValidation()
	}
	var wire struct {
		Version *int64            `json:"version"`
		Source  json.RawMessage   `json:"source"`
		Entries []json.RawMessage `json:"entries"`
	}
	if !mapImportFields(raw, "version", "source", "entries") || json.Unmarshal(raw, &wire) != nil || wire.Version == nil || *wire.Version != 1 || wire.Entries == nil || len(wire.Entries) < 1 || len(wire.Entries) > 100 {
		return d, mapImportValidation()
	}
	if decodeReviewResponse(wire.Source, &d.Source) != nil {
		return d, mapImportValidation()
	}
	d.Version = 1
	s := &d.Source
	if !ValidMapImportSourceKey(s.Key) || s.RepositoryID < 1 || s.Kind != "prepared" || !noteText(s.Snapshot, 512, true, true) || !noteText(s.Scope, 1024, true, true) || s.TransformationVersion != 1 || len(s.CollectedAt) > 64 {
		return d, mapImportValidation()
	}
	at, e := time.Parse(time.RFC3339Nano, s.CollectedAt)
	if e != nil || at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return d, mapImportValidation()
	}
	s.CollectedAt = at.UTC().Format(time.RFC3339Nano)
	if at.UTC().Year() < 1 || at.UTC().Year() > 9999 {
		return d, mapImportValidation()
	}
	d.Entries = make([]MapImportItem, 0, len(wire.Entries))
	seen := map[string]bool{}
	for _, rawItem := range wire.Entries {
		var item struct {
			SourceKey        *string         `json:"source_key"`
			ExpectedRevision *int64          `json:"expected_revision"`
			Entry            json.RawMessage `json:"entry"`
			Provenance       json.RawMessage `json:"provenance"`
		}
		if !mapImportFields(rawItem, "source_key", "expected_revision", "entry", "provenance") || json.Unmarshal(rawItem, &item) != nil || item.SourceKey == nil || !ValidMapImportSourceKey(*item.SourceKey) || seen[*item.SourceKey] || item.ExpectedRevision == nil || *item.ExpectedRevision < 0 || *item.ExpectedRevision == math.MaxInt64 {
			return d, mapImportValidation()
		}
		entry, err := DecodeProjectMapEditable(item.Entry)
		if err != nil {
			return d, mapImportValidation()
		}
		var p struct {
			References  []string `json:"references"`
			Inferred    *bool    `json:"inferred"`
			Confidence  *string  `json:"confidence,omitempty"`
			Uncertainty *string  `json:"uncertainty,omitempty"`
		}
		if strictjson.Decode(item.Provenance, &p) != nil || p.Inferred == nil || len(p.References) > 20 {
			return d, mapImportValidation()
		}
		// Optional omission selects defaults; explicit null is never approval.
		var fields map[string]json.RawMessage
		if json.Unmarshal(item.Provenance, &fields) != nil || fields == nil {
			return d, mapImportValidation()
		}
		for name, v := range fields {
			if name != "references" && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return d, mapImportValidation()
			}
		}
		confidence, uncertainty := "unknown", ""
		if p.Confidence != nil {
			confidence = *p.Confidence
		}
		if p.Uncertainty != nil {
			uncertainty = *p.Uncertainty
		}
		if confidence == "" {
			confidence = "unknown"
		}
		if confidence != "unknown" && confidence != "low" && confidence != "medium" && confidence != "high" || !noteText(uncertainty, 4096, false, true) {
			return d, mapImportValidation()
		}
		for _, ref := range p.References {
			u, e := url.Parse(ref)
			if !noteText(ref, 2048, true, false) || strings.IndexFunc(ref, unicode.IsSpace) >= 0 || e != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Scheme != "http" && u.Scheme != "https" {
				return d, mapImportValidation()
			}
		}
		d.Entries = append(d.Entries, MapImportItem{*item.SourceKey, *item.ExpectedRevision, entry, MapImportProvenance{append([]string{}, p.References...), *p.Inferred, confidence, uncertainty}})
		seen[*item.SourceKey] = true
	}
	wrapped, err := mapImportWrapper(strings.Repeat("0", 64), d)
	if err != nil || len(wrapped) > maxMapImportDocument {
		return d, mapImportValidation()
	}
	return d, nil
}
func ValidateMapImportDocument(raw []byte) error { _, err := DecodeMapImportDocument(raw); return err }
func mapImportApplyBody(key, hash string, raw []byte) (MapImportDocument, []byte, error) {
	if !validMutationKey(key) || !controlHex(hash, 64) {
		return MapImportDocument{}, nil, mutationValidation()
	}
	d, err := DecodeMapImportDocument(raw)
	if err != nil {
		return d, nil, err
	}
	body, err := mapImportWrapper(hash, d)
	if err != nil {
		return d, nil, mapImportValidation()
	}
	return d, body, nil
}
func ValidateMapImportApply(key, hash string, raw []byte) error {
	_, _, err := mapImportApplyBody(key, hash, raw)
	return err
}
func (c *Client) mapImportPath(parts ...string) []string {
	return c.projectPath(append([]string{"map", "imports"}, parts...)...)
}
func canonicalMapImportDocument(d MapImportDocument) bool {
	raw, e := json.Marshal(d)
	if e != nil {
		return false
	}
	normalized, e := DecodeMapImportDocument(raw)
	if e != nil {
		return false
	}
	b, _ := json.Marshal(normalized)
	return bytes.Equal(raw, b)
}
func (c *Client) PreviewMapImport(ctx context.Context, raw []byte) (MapImportPreview, error) {
	d, err := DecodeMapImportDocument(raw)
	if err != nil {
		return MapImportPreview{}, err
	}
	body, _ := json.Marshal(d)
	var v MapImportPreview
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("map", "import", "preview"), nil, true, "", body, http.StatusOK, false, &v, 512<<10, decodeReviewResponse); err != nil {
		return v, err
	}
	a, _ := json.Marshal(d)
	b, _ := json.Marshal(v.Document)
	if !bytes.Equal(a, b) || !canonicalMapImportDocument(v.Document) || v.Plan == nil || len(v.Plan) != len(d.Entries) || v.Conflicts < 0 || v.Conflicts > int64(len(v.Plan)) {
		return MapImportPreview{}, invalidResponse()
	}
	var conflicts int64
	seenTargets := map[string]bool{}
	for i, p := range v.Plan {
		if p.SourceKey != d.Entries[i].SourceKey || p.CurrentRevision < 0 || !noteText(p.Reason, 128, true, false) {
			return MapImportPreview{}, invalidResponse()
		}
		switch p.Action {
		case "create":
			if p.EntryID != "" || p.CurrentRevision != 0 || d.Entries[i].ExpectedRevision != 0 {
				return MapImportPreview{}, invalidResponse()
			}
		case "update", "unchanged":
			expected := d.Entries[i].ExpectedRevision
			retainedReplay := p.Action == "unchanged" && p.Reason == "retained_document_unchanged" && expected > 0 && p.CurrentRevision > expected
			if !mapImportEntryID(p.EntryID) || p.CurrentRevision < 1 || expected > 0 && p.CurrentRevision != expected && !retainedReplay || p.Action == "update" && expected == 0 || p.Reason == "retained_document_unchanged" && !retainedReplay {
				return MapImportPreview{}, invalidResponse()
			}
		case "conflict":
			conflicts++
			if (p.EntryID == "") != (p.CurrentRevision == 0) || p.EntryID != "" && !mapImportEntryID(p.EntryID) {
				return MapImportPreview{}, invalidResponse()
			}
		default:
			return MapImportPreview{}, invalidResponse()
		}
		if p.EntryID != "" {
			if seenTargets[p.EntryID] {
				return MapImportPreview{}, invalidResponse()
			}
			seenTargets[p.EntryID] = true
		}
	}
	if conflicts != v.Conflicts || v.Ready != (conflicts == 0) || v.Ready && !controlHex(v.PreviewSHA256, 64) || !v.Ready && v.PreviewSHA256 != "" {
		return MapImportPreview{}, invalidResponse()
	}
	return v, nil
}
func (c *Client) ApplyMapImport(ctx context.Context, key, hash string, raw []byte) (MapImportReceipt, error) {
	d, body, err := mapImportApplyBody(key, hash, raw)
	if err != nil {
		return MapImportReceipt{}, err
	}
	var v MapImportReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("map", "import"), nil, true, key, body, http.StatusOK, true, &v, 4<<10, decodeReviewResponse); err != nil {
		var refusal *Error
		if errors.As(err, &refusal) && !refusal.OutcomeUnknown && refusal.Status == http.StatusConflict && (refusal.Code == "import_preview_mismatch" || refusal.Code == "map_import_conflict") {
			safe := *refusal
			safe.Action = "Preview the selected map document again and inspect its plan before applying."
			return MapImportReceipt{}, &safe
		}
		return v, err
	}
	if v.SourceKey != d.Source.Key || v.Revision < 1 || v.Created < 0 || v.Updated < 0 || v.Unchanged < 0 || v.Created > 100 || v.Updated > 100 || v.Unchanged > 100 || v.Created+v.Updated+v.Unchanged != int64(len(d.Entries)) || v.Outcome != "imported" && v.Outcome != "unchanged" || v.Outcome == "unchanged" && (v.Created != 0 || v.Updated != 0) {
		return MapImportReceipt{}, unknownMutation("invalid_response", "The service returned an invalid map import receipt; its outcome is unknown.", http.StatusOK)
	}
	return v, nil
}
func validMapImportRecord(v MapImportRecord, key string) bool {
	if v.SourceKey != key || v.Document.Source.Key != key || v.Revision < 1 || !validID(v.ActorID) || !validID(v.WorkerID) || !validJobProgressTime(v.CreatedAt) || !canonicalMapImportDocument(v.Document) || v.Mappings == nil || len(v.Mappings) != len(v.Document.Entries) {
		return false
	}
	seen := map[string]bool{}
	for i, m := range v.Mappings {
		if m.SourceKey != v.Document.Entries[i].SourceKey || !mapImportEntryID(m.EntryID) || seen[m.EntryID] || m.EntryRevision < 1 || m.SourceRevision != v.Revision {
			return false
		}
		seen[m.EntryID] = true
	}
	return true
}
func (c *Client) MapImport(ctx context.Context, key string) (MapImportRecord, error) {
	if !ValidMapImportSourceKey(key) {
		return MapImportRecord{}, validation()
	}
	var v MapImportRecord
	if err := c.requestBounded(ctx, http.MethodGet, c.mapImportPath(key), nil, true, "", nil, http.StatusOK, false, &v, 512<<10, decodeReviewResponse); err != nil {
		return v, err
	}
	if !validMapImportRecord(v, key) {
		return MapImportRecord{}, invalidResponse()
	}
	return v, nil
}
func (c *Client) MapImportHistory(ctx context.Context, key string, options PageOptions) (Page[MapImportHistorySummary], error) {
	options, q, err := projectMapPage(options)
	if err != nil || !ValidMapImportSourceKey(key) {
		return Page[MapImportHistorySummary]{}, validation()
	}
	var v Page[MapImportHistorySummary]
	if err = c.requestBounded(ctx, http.MethodGet, c.mapImportPath(key, "history"), q, true, "", nil, http.StatusOK, false, &v, 64<<10, decodeReviewResponse); err != nil {
		return v, err
	}
	if !validPage(v, options) || v.Cursor != "" && !validOpaqueCursor(v.Cursor) {
		return Page[MapImportHistorySummary]{}, invalidResponse()
	}
	var previous int64
	for _, h := range v.Items {
		if h.Revision < 1 || previous != 0 && h.Revision >= previous || !validID(h.ActorID) || !validID(h.WorkerID) || !validJobProgressTime(h.CreatedAt) {
			return Page[MapImportHistorySummary]{}, invalidResponse()
		}
		previous = h.Revision
	}
	return v, nil
}
func (c *Client) MapImportRevision(ctx context.Context, key string, revision int64) (MapImportRecord, error) {
	if !ValidMapImportSourceKey(key) || revision < 1 {
		return MapImportRecord{}, validation()
	}
	var v MapImportRecord
	if err := c.requestBounded(ctx, http.MethodGet, c.mapImportPath(key, "history", strconv.FormatInt(revision, 10)), nil, true, "", nil, http.StatusOK, false, &v, 512<<10, decodeReviewResponse); err != nil {
		return v, err
	}
	if !validMapImportRecord(v, key) || v.Revision != revision {
		return MapImportRecord{}, invalidResponse()
	}
	return v, nil
}
