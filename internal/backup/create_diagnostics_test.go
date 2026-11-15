package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func creationLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&b, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &b
}
func phaseRecords(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "phase": true, "duration_ms": true, "database_bytes": true, "captures_copied": true, "capture_bytes_copied": true, "failed": true, "deadline_exceeded": true, "cancelled": true}
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		for k := range r {
			if !allowed[k] {
				t.Fatal("unexpected field", k)
			}
		}
		out = append(out, r)
	}
	return out
}
func TestCreationDiagnosticsCancelledPreservesError(t *testing.T) {
	b := creationLogs(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := createBundleWithClock(ctx, nil, "private-id", "private-source", "private-destination", time.Now, time.Since)
	backupCode(t, err, "backup_unavailable")
	records := phaseRecords(t, b)
	if len(records) != 2 || records[1]["phase"] != "preflight" || records[1]["failed"] != true || records[1]["cancelled"] != true {
		t.Fatal(records)
	}
	if strings.Contains(b.String(), "private-") {
		t.Fatal("private data leaked")
	}
}
func TestCreationDiagnosticsDeadlineClassification(t *testing.T) {
	b := creationLogs(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	d := beginCreateDiagnostics(ctx, "growth_admission")
	d.finish(unavailable())
	r := phaseRecords(t, b)[1]
	if r["deadline_exceeded"] != true || r["cancelled"] != false || r["failed"] != true {
		t.Fatal(r)
	}
}
func TestCreationDiagnosticsCompletePipeline(t *testing.T) {
	f := newRestoreFixture(t)
	b := creationLogs(t)
	result, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	records := phaseRecords(t, b)
	for _, r := range records {
		if r["msg"] == "Backup creation phase outcome" {
			phases = append(phases, r["phase"].(string))
			if r["failed"] != false || r["duration_ms"].(float64) < 0 {
				t.Fatal(r)
			}
		}
	}
	want := []string{"preflight", "source_validation", "staging", "snapshot_vacuum", "database_hash", "capture_copy", "snapshot_metadata", "full_validation", "seal", "publication"}
	if !reflect.DeepEqual(phases, want) || len(records) != 2*len(want) {
		t.Fatal(phases, len(records))
	}
	last := records[len(records)-1]
	if last["database_bytes"] != float64(result.DatabaseBytes) || last["captures_copied"] != float64(result.ArtifactCount) {
		t.Fatal(last, result)
	}
}

func TestCreationDiagnosticsNilContextPreservesRefusal(t *testing.T) {
	b := creationLogs(t)
	_, err := createBundleWithClock(nil, nil, "private-id", "private-source", "private-destination", time.Now, time.Since)
	backupCode(t, err, "backup_unavailable")
	r := phaseRecords(t, b)[1]
	if r["failed"] != true || r["cancelled"] != false {
		t.Fatal(r)
	}
}
