package backup

import (
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/retention"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestReviewRetentionInspectAndInactiveRestore(t *testing.T) {
	f := historyInventoryAPIFixture(t)
	ctx := t.Context()
	if err := jobs.EnsureSchema(ctx, f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviewexec.EnsureSchema(ctx, f.store); err != nil {
		t.Fatal(err)
	}
	var repository int64
	if err := f.store.DB().QueryRow(`SELECT github_repository_id FROM projects WHERE id=?`, f.project).Scan(&repository); err != nil {
		t.Fatal(err)
	}
	var snapshots []string
	prefix := "/api/v1/projects/" + f.project + "/reviews"
	for i, title := range []string{"Old discardable title", "Floor title", "Latest title"} {
		reply := f.request(t, http.MethodPost, prefix+"/observations", map[string]any{
			"expected_revision": i,
			"repository_id":     repository,
			"source":            "retention-fixture",
			"observed_at":       time.Now().UTC().Format(time.RFC3339Nano),
			"queue":             []map[string]any{{"number": 1, "title": title, "state": "open", "head_sha": strings.Repeat("a", 40), "base_ref": "main", "base_sha": strings.Repeat("b", 40), "ci_state": "success", "mergeability": "mergeable", "complete": true, "runs": []any{}, "attempts": []any{}}},
		})
		snapshots = append(snapshots, reply["id"].(string))
	}
	now := time.Now().UTC()
	if _, err := f.store.DB().Exec(`UPDATE review_snapshots SET recorded_at=? WHERE project_id=? AND revision<=2`, now.Add(-31*24*time.Hour).Format(time.RFC3339Nano), f.project); err != nil {
		t.Fatal(err)
	}
	if err := f.store.EnsureIncrementalVacuum(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := retention.Config{Interval: retention.DefaultInterval, ReviewHistory: retention.DefaultHistory, JobHistory: retention.DefaultHistory, Receipts: retention.DefaultHistory, Diagnostics: retention.DefaultDiagnostics}
	report, skipped, err := retention.RunPass(ctx, f.store, cfg, now)
	if err != nil || skipped || report.Deleted["review_snapshots"] != 1 {
		t.Fatal(report, skipped, err)
	}
	var floor string
	if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key=?`, core.ReviewHistoryFloorPrefix+f.project).Scan(&floor); err != nil || floor != "2" {
		t.Fatal(floor, err)
	}
	expected := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err = Inspect(ctx, bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	result, err := Restore(ctx, bundle.Path, destination)
	if err != nil || result.State != "inactive" {
		t.Fatal(result, err)
	}
	// Inactive restore rotates instance metadata, not historical visibility.
	got := generationRead(t, destination, `SELECT key,value FROM metadata WHERE key GLOB 'retention:*' ORDER BY key`)
	if !reflect.DeepEqual(got, restoreRows(t, f.store.DB(), `SELECT key,value FROM metadata WHERE key GLOB 'retention:*' ORDER BY key`)) {
		t.Fatal("retention metadata changed on inactive restore", got)
	}
	if !reflect.DeepEqual(expected, backupRows(t, f.store.DB())) {
		t.Fatal("Inspect/Restore altered original database")
	}
	if retained := generationRead(t, destination, `SELECT revision FROM review_snapshots ORDER BY revision`); !reflect.DeepEqual(retained, [][]any{{int64(2)}, {int64(3)}}) {
		t.Fatal("unexpected restored history window", retained, snapshots)
	}
}
