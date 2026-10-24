package projectmap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

const MaxImportBytes = 256 << 10
const MaxImportRecordBytes = 512 << 10

var importKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type ImportSource struct {
	Key                   string `json:"key"`
	RepositoryID          int64  `json:"repository_id"`
	Kind                  string `json:"kind"`
	Snapshot              string `json:"snapshot"`
	CollectedAt           string `json:"collected_at"`
	TransformationVersion int    `json:"transformation_version"`
	Scope                 string `json:"scope"`
}
type ImportProvenance struct {
	References  []string `json:"references"`
	Inferred    bool     `json:"inferred"`
	Confidence  string   `json:"confidence"`
	Uncertainty string   `json:"uncertainty"`
}
type ImportItem struct {
	SourceKey        string           `json:"source_key"`
	ExpectedRevision int64            `json:"expected_revision"`
	Entry            Editable         `json:"entry"`
	Provenance       ImportProvenance `json:"provenance"`
}
type ImportDocument struct {
	Version int          `json:"version"`
	Source  ImportSource `json:"source"`
	Entries []ImportItem `json:"entries"`
}
type ImportPlanItem struct {
	SourceKey       string `json:"source_key"`
	Action          string `json:"action"`
	EntryID         string `json:"entry_id"`
	CurrentRevision int64  `json:"current_revision"`
	Reason          string `json:"reason"`
}
type ImportPreview struct {
	Document      ImportDocument   `json:"document"`
	Ready         bool             `json:"ready"`
	PreviewSHA256 string           `json:"preview_sha256"`
	Plan          []ImportPlanItem `json:"plan"`
	Conflicts     int              `json:"conflicts"`
}
type ImportApplyRequest struct {
	Document      ImportDocument `json:"document"`
	PreviewSHA256 string         `json:"preview_sha256"`
}
type ImportReceipt struct {
	SourceKey string `json:"source_key"`
	Revision  int64  `json:"revision"`
	Outcome   string `json:"outcome"`
	Created   int    `json:"created"`
	Updated   int    `json:"updated"`
	Unchanged int    `json:"unchanged"`
}
type ImportMapping struct {
	SourceKey      string `json:"source_key"`
	EntryID        string `json:"entry_id"`
	EntryRevision  int64  `json:"entry_revision"`
	SourceRevision int64  `json:"source_revision"`
}
type ImportRecord struct {
	SourceKey string          `json:"source_key"`
	Revision  int64           `json:"revision"`
	Document  ImportDocument  `json:"document"`
	Mappings  []ImportMapping `json:"mappings"`
	ActorID   string          `json:"actor_id"`
	WorkerID  string          `json:"worker_id"`
	CreatedAt string          `json:"created_at"`
}
type ImportHistorySummary struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	WorkerID  string `json:"worker_id"`
	CreatedAt string `json:"created_at"`
}

func requiredImportFields(data []byte, names ...string) error {
	if err := nonnullScalars(data); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return invalid()
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return invalid()
		}
	}
	return nil
}
func (s *ImportSource) UnmarshalJSON(data []byte) error {
	type plain ImportSource
	var value plain
	if err := requiredImportFields(data, "key", "repository_id", "kind", "snapshot", "collected_at", "transformation_version", "scope"); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = ImportSource(value)
	return nil
}
func (p *ImportProvenance) UnmarshalJSON(data []byte) error {
	type plain ImportProvenance
	value := plain{Confidence: "unknown"}
	if err := nonnullScalars(data, "references"); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields["inferred"] == nil {
		return invalid()
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value.References == nil {
		value.References = []string{}
	}
	*p = ImportProvenance(value)
	return nil
}
func (i *ImportItem) UnmarshalJSON(data []byte) error {
	var value struct {
		SourceKey        string            `json:"source_key"`
		ExpectedRevision *int64            `json:"expected_revision"`
		Entry            *editableRequest  `json:"entry"`
		Provenance       *ImportProvenance `json:"provenance"`
	}
	if err := requiredImportFields(data, "source_key", "expected_revision", "entry", "provenance"); err != nil {
		return err
	}
	if err := strictjson.Decode(data, &value); err != nil {
		return err
	}
	if value.ExpectedRevision == nil || value.Entry == nil || value.Provenance == nil {
		return invalid()
	}
	e, err := value.Entry.value()
	if err != nil {
		return err
	}
	*i = ImportItem{value.SourceKey, *value.ExpectedRevision, e, *value.Provenance}
	return nil
}
func DecodeImportDocument(data []byte) (ImportDocument, error) {
	var doc ImportDocument
	if len(data) > MaxImportBytes || strictjson.Decode(data, &doc) != nil {
		return ImportDocument{}, invalid()
	}
	return NormalizeImportDocument(doc)
}
func NormalizeImportDocument(doc ImportDocument) (ImportDocument, error) {
	if doc.Version != 1 || !importKey.MatchString(doc.Source.Key) || doc.Source.RepositoryID < 1 || doc.Source.Kind != "prepared" || doc.Source.TransformationVersion != 1 || !text(doc.Source.Snapshot, 512) || strings.TrimSpace(doc.Source.Snapshot) == "" || !text(doc.Source.Scope, 1024) || strings.TrimSpace(doc.Source.Scope) == "" || len(doc.Entries) < 1 || len(doc.Entries) > 100 {
		return ImportDocument{}, invalid()
	}
	collected, err := time.Parse(time.RFC3339Nano, doc.Source.CollectedAt)
	if err != nil || collected.IsZero() || len(doc.Source.CollectedAt) > 64 || collected.UTC().Year() < 1 || collected.UTC().Year() > 9999 {
		return ImportDocument{}, invalid()
	}
	doc.Source.CollectedAt = collected.UTC().Format(time.RFC3339Nano)
	seen := map[string]bool{}
	// Own normalized slices so callers cannot change the approved document by
	// mutating an input collection after normalization.
	doc.Entries = append([]ImportItem{}, doc.Entries...)
	for n := range doc.Entries {
		item := &doc.Entries[n]
		if !importKey.MatchString(item.SourceKey) || seen[item.SourceKey] || item.ExpectedRevision < 0 || item.ExpectedRevision == math.MaxInt64 {
			return ImportDocument{}, invalid()
		}
		seen[item.SourceKey] = true
		e := &item.Entry
		e.Title = strings.TrimSpace(e.Title)
		e.Phases = append([]Phase{}, e.Phases...)
		e.Links = append([]Link{}, e.Links...)
		for n := range e.Phases {
			e.Phases[n].Name = strings.TrimSpace(e.Phases[n].Name)
		}
		for n := range e.Links {
			e.Links[n].Label = strings.TrimSpace(e.Links[n].Label)
		}
		if validateEditable(*e) != nil {
			return ImportDocument{}, invalid()
		}
		p := &item.Provenance
		p.References = append([]string{}, p.References...)
		if p.Confidence == "" {
			p.Confidence = "unknown"
		}
		if p.Confidence != "unknown" && p.Confidence != "low" && p.Confidence != "medium" && p.Confidence != "high" || !text(p.Uncertainty, 4096) || len(p.References) > 20 {
			return ImportDocument{}, invalid()
		}
		for _, ref := range p.References {
			u, err := url.Parse(ref)
			if len(ref) > 2048 || !utf8.ValidString(ref) || strings.IndexFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 || err != nil || u == nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") {
				return ImportDocument{}, invalid()
			}
		}
	}
	wrapper, err := json.Marshal(ImportApplyRequest{doc, strings.Repeat("0", 64)})
	if err != nil || len(wrapper) > MaxImportBytes {
		return ImportDocument{}, invalid()
	}
	return doc, nil
}
func importFingerprint(instance string, scope core.Scope, doc ImportDocument) (string, error) {
	body, err := json.Marshal(struct {
		Domain    string         `json:"domain"`
		Instance  string         `json:"instance"`
		Project   string         `json:"project"`
		Principal string         `json:"principal"`
		Worker    string         `json:"worker"`
		Document  ImportDocument `json:"document"`
	}{"firecontroller.map-import.preview/v1", instance, scope.ProjectID, scope.PrincipalID, scope.WorkerID, doc})
	if err != nil {
		return "", invalid()
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
func sameImportDocument(a, b ImportDocument) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
