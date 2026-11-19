package backup

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestJobHistoryGapsPassInspectAndInactiveRestore(t *testing.T) {
	f := newRestoreFixture(t)
	job := seedBackupJobSummaries(t, f)
	seedBackupNotes(t, f)
	f.request(t, "PATCH", "/api/v1/projects/"+f.project+"/jobs/"+job, map[string]any{
		"expected_revision": 2, "brief": "Current revision remains available.",
	})
	history := func(revision string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/jobs/"+job+"/history/"+revision, nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("X-FC-Instance", f.store.InstanceID())
		req.Header.Set("X-FC-Worker", f.worker)
		result := httptest.NewRecorder()
		f.handler.ServeHTTP(result, req)
		return result.Code, result.Body.String()
	}
	firstStatus, firstBefore := history("1")
	currentStatus, currentBefore := history("3")
	if firstStatus != http.StatusOK || currentStatus != http.StatusOK {
		t.Fatalf("baseline revision responses: first=%d current=%d", firstStatus, currentStatus)
	}
	// Construct the exact shape retention produces: a gap in each
	// independently revisioned archive, including a summary pointing at a
	// past job revision that no longer has a full-state snapshot.
	if _, err := f.store.DB().Exec("DELETE FROM job_history WHERE job_id=? AND revision=2; DELETE FROM job_summary_history WHERE revision=2; DELETE FROM note_revisions WHERE revision=2", job); err != nil {
		t.Fatal(err)
	}
	if status, body := history("1"); status != http.StatusOK || body != firstBefore {
		t.Fatalf("first retained revision changed: %d %s", status, body)
	}
	if status, body := history("3"); status != http.StatusOK || body != currentBefore {
		t.Fatalf("current retained revision changed: %d %s", status, body)
	}
	if status, body := history("2"); status != http.StatusNotFound ||
		!strings.Contains(body, "The requested retained job revision was not found.") {
		t.Fatalf("pruned revision should return the existing 404: %d %s", status, body)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	result, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || result.State != "inactive" {
		t.Fatalf("inactive restore: %+v %v", result, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"jobs", "job_history", "job_progress", "job_progress_history", "job_summaries", "job_summary_history", "notes", "note_revisions"} {
		if before[table] != after[table] {
			t.Fatalf("%s changed during restore", table)
		}
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("source changed during restore")
	}
}

func TestJobHistoryInspectRefusesUnretainedCurrentOrInvalidPresentRow(t *testing.T) {
	f := newRestoreFixture(t)
	seedBackupJobSummaries(t, f)
	for _, query := range []string{
		"DELETE FROM job_history WHERE revision=2",
		"UPDATE job_history SET state_json='{}' WHERE revision=1",
	} {
		t.Run(query, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, query)
			assertSharedProjectBundleRefused(t, bundle.Path)
		})
	}
}
