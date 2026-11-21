package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
)

func TestWorkerRetirementNoteBlockersUseSelectivePlan(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := jobs.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	rows, err := store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+workerRetirementPendingNotes+" ORDER BY id LIMIT 20", "project", "worker", "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, seek := range []string{
		"notes_project_worker_created (project_id=? AND worker_id=?)",
		"jobs_project_worker_order (project_id=? AND worker_id=?)",
		"notes_project_job_created (project_id=? AND job_id=?)",
	} {
		if !strings.Contains(plan.String(), seek) {
			t.Fatalf("missing worker-scoped seek %s:\n%s", seek, plan.String())
		}
	}
}
