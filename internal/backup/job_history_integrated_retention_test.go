package backup

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/retention"
)

func TestJobHistoryIntegratedRetentionInspectAndInactiveRestore(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := t.Context()
	job := restoreRows(t, f.store.DB(), "SELECT id FROM jobs LIMIT 1")[0][0].(string)
	jobPath := "/api/v1/projects/" + f.project + "/jobs/" + job
	note := seedBackupNotes(t, f)
	notePath := "/api/v1/projects/" + f.project + "/notes/" + note
	for revision := 1; revision <= 30; revision++ {
		body := fmt.Sprintf("Synthetic retained revision %d", revision)
		if revision > 1 {
			f.request(t, "PATCH", jobPath, map[string]any{"expected_revision": revision - 1, "brief": body})
		}
		f.request(t, "PUT", jobPath+"/summary", map[string]any{
			"expected_job_revision": revision, "expected_revision": revision - 1, "title": "Synthetic summary", "body": body,
		})
		if revision > 4 {
			f.request(t, "PATCH", notePath, map[string]any{"expected_revision": revision - 1, "body": body})
		}
	}
	now := time.Now().UTC()
	createdAt := now.Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
	progressID := func(sequence int) string { return fmt.Sprintf("job_progress_synthetic_%02d", sequence) }
	// Imported checkpoints can share the same underlying job revision. Model
	// that legitimate state in the genuine schema, including repeated rev-1
	// entries, and make the canonical current row equal to the newest entry.
	for sequence := 1; sequence <= 61; sequence++ {
		progressAt := createdAt
		if sequence == 4 {
			// Recent entry on old rev 2 protects its old sibling too.
			progressAt = now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
		}
		revision := (sequence + 1) / 2
		if sequence == 61 {
			// Later append is bound to an older imported owning revision.
			revision = 5
		}
		if _, err := f.store.DB().ExecContext(ctx, "INSERT INTO job_progress_history(id,project_id,job_id,job_revision,actor_id,worker_id,summary,next,blocker,evidence_json,created_at) VALUES(?,?,?,?,?,?,?,'','','[]',?)",
			progressID(sequence), f.project, job, revision, f.principal, f.worker, fmt.Sprintf("Archived progress %d", sequence), progressAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.DB().ExecContext(ctx, "UPDATE job_progress SET job_revision=5,summary='Archived progress 61' WHERE project_id=? AND job_id=?", f.project, job); err != nil {
		t.Fatal(err)
	}
	// Simulate age instead of passing a future worker clock, since the
	// backup validator requires the actual worker report to finish after it starts.
	for _, statement := range []string{
		"UPDATE job_history SET created_at=?",
		"UPDATE job_summary_history SET created_at=?",
		"UPDATE job_summaries SET created_at=?",
		"UPDATE note_revisions SET created_at=?,updated_at=?,recorded_at=?",
		"UPDATE notes SET created_at=?,updated_at=?",
	} {
		args := make([]any, strings.Count(statement, "?"))
		for i := range args {
			args[i] = createdAt
		}
		if _, err := f.store.DB().ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, offset := range []int{0, 1} {
		if offset == 1 {
			if err := f.store.EnsureIncrementalVacuum(ctx); err != nil {
				t.Fatal(err)
			}
		}
		config := retention.Config{
			Interval: retention.DefaultInterval, JobHistory: retention.DefaultHistory, DryRun: offset == 0,
			ReviewHistory: 365 * 24 * time.Hour, Receipts: 365 * 24 * time.Hour, Diagnostics: 365 * 24 * time.Hour,
		}
		report, skipped, err := retention.RunPass(ctx, f.store, config, now)
		if skipped || err != nil {
			t.Fatalf("integrated retention skipped=%v report=%+v: %v", skipped, report, err)
		}
		for _, table := range []string{"job_history", "job_progress_history", "job_summary_history", "note_revisions"} {
			want := int64(9)
			if table == "job_progress_history" {
				want = 14
			}
			if report.Deleted[table] != want {
				t.Fatalf("dry_run=%v %s: %d eligible/deleted; want %d", config.DryRun, table, report.Deleted[table], want)
			}
		}
		if config.DryRun && len(restoreRows(t, f.store.DB(), "SELECT id FROM job_progress_history")) != 61 {
			t.Fatal("dry-run removed progress rows")
		}
	}
	progress := restoreRows(t, f.store.DB(), "SELECT id, job_revision FROM job_progress_history ORDER BY sequence")
	var wantProgressIDs []string
	for sequence := 1; sequence <= 61; sequence++ {
		// Keep every rev-1/2/5 row and both entries of every revision 11–30.
		if sequence <= 4 || sequence == 9 || sequence == 10 || sequence >= 21 {
			wantProgressIDs = append(wantProgressIDs, progressID(sequence))
		}
	}
	var gotProgressIDs []string
	for _, row := range progress {
		gotProgressIDs = append(gotProgressIDs, row[0].(string))
	}
	if !reflect.DeepEqual(gotProgressIDs, wantProgressIDs) {
		t.Fatalf("progress eligibility by owning revision: got %v want %v", gotProgressIDs, wantProgressIDs)
	}
	current := restoreRows(t, f.store.DB(), "SELECT job_revision, summary FROM job_progress")
	if len(current) != 1 || current[0][0] != int64(5) || current[0][1] != "Archived progress 61" {
		t.Fatalf("latest current progress changed: %v", current)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(ctx, bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	restored, err := Restore(ctx, bundle.Path, destination)
	if err != nil || restored.State != "inactive" {
		t.Fatalf("inactive restore: %+v %v", restored, err)
	}
	if got := generationRead(t, destination, "SELECT id, job_revision FROM job_progress_history ORDER BY sequence"); !reflect.DeepEqual(got, progress) {
		t.Fatalf("restored progress archive changed: %v", got)
	}
	if got := generationRead(t, destination, "SELECT job_revision, summary FROM job_progress"); !reflect.DeepEqual(got, current) {
		t.Fatalf("restored current progress changed: %v", got)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("source changed during Inspect or Restore")
	}
}
