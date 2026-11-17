package backup

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewOnlyIdentityBackupRestorePreservesSelectionAndEvidence(t *testing.T) {
	f := historyInventoryAPIFixture(t)
	input := reviews.SubagentRecordRequest{PullNumber: 99, HeadSHA: strings.Repeat("a", 40), Reviewer: "Independent worker", Scope: "Maintenance", StartedAt: "2026-01-01T01:00:00Z", FinishedAt: "2026-01-01T01:01:00Z", Findings: []reviews.SubagentFindingInput{}, Finalized: true}
	receipt := f.request(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/subagent-reports", input)
	if receipt["id"] == "" {
		t.Fatal("no receipt")
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"review_pull_versions", "review_run_versions", "review_pull_memberships", "review_run_memberships", "review_current_pulls", "review_workspace", "review_snapshots"} {
		if before[table] != after[table] {
			t.Fatal("restore changed retained evidence", table)
		}
	}
	var selected int
	if err := db.QueryRow(`SELECT delivery_selected FROM review_pull_versions WHERE project_id=? AND number=99`, f.project).Scan(&selected); err != nil || selected != 0 {
		t.Fatal("restored review was enrolled", selected, err)
	}
}
