package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validationRecords(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "phase": true, "duration_ms": true, "failed": true, "deadline_exceeded": true, "cancelled": true}
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(r["msg"].(string), "Backup validation phase ") {
			continue
		}
		for k := range r {
			if !allowed[k] {
				t.Fatal("unexpected field", k)
			}
		}
		records = append(records, r)
	}
	return records
}
func TestValidationDiagnosticsCreationAndInspection(t *testing.T) {
	f := newRestoreFixture(t)
	b := creationLogs(t)
	result, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		records := validationRecords(t, b)
		if len(records) != 42 {
			t.Fatalf("expected bounded 21 phases, got %d records", len(records))
		}
		seen := map[any]bool{}
		for i := 0; i < len(records); i += 2 {
			start, outcome := records[i], records[i+1]
			if start["msg"] != "Backup validation phase started" || outcome["msg"] != "Backup validation phase outcome" || start["phase"] != outcome["phase"] || seen[start["phase"]] || outcome["failed"] != false || outcome["duration_ms"].(float64) < 0 {
				t.Fatal(start, outcome)
			}
			seen[start["phase"]] = true
		}
		if strings.Contains(b.String(), f.dir) || strings.Contains(b.String(), result.ID) || strings.Contains(b.String(), f.token) {
			t.Fatal("private data leaked")
		}
	}
	check()
	b.Reset()
	if _, err = Inspect(t.Context(), result.Path); err != nil {
		t.Fatal(err)
	}
	check()
	// Preserve the existing semantic refusal while reporting the failing phase.
	m, err := inspectBundleIntegrity(t.Context(), result.Path)
	if err != nil {
		t.Fatal(err)
	}
	m.SourceInstanceID = "private-invalid-instance"
	b.Reset()
	err = validateSnapshotContent(t.Context(), result.Path, m, false)
	backupCode(t, err, "backup_invalid")
	r := validationRecords(t, b)
	last := r[len(r)-1]
	if last["phase"] != "administration" || last["failed"] != true || strings.Contains(b.String(), m.SourceInstanceID) {
		t.Fatal(last)
	}
}
func TestValidationDiagnosticsCancelledPreservesError(t *testing.T) {
	b := creationLogs(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := validateSnapshotContent(ctx, filepath.Join(t.TempDir(), "private-path"), Manifest{}, false)
	backupCode(t, err, "backup_unavailable")
	r := validationRecords(t, b)
	if len(r) != 2 || r[1]["failed"] != true || r[1]["cancelled"] != true || strings.Contains(b.String(), "private-path") {
		t.Fatal(r)
	}
}

func TestValidationDiagnosticsDeadlinePreservesError(t *testing.T) {
	b := creationLogs(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	err := validateSnapshotContent(ctx, filepath.Join(t.TempDir(), "private-path"), Manifest{}, false)
	backupCode(t, err, "backup_unavailable")
	r := validationRecords(t, b)
	if len(r) != 2 || r[1]["failed"] != true || r[1]["deadline_exceeded"] != true || r[1]["cancelled"] != false || strings.Contains(b.String(), "private-path") {
		t.Fatal(r)
	}
}
