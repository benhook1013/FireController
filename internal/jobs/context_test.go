package jobs_test

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
)

func contextState(t *testing.T, a *testAPI) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, table := range []string{"jobs", "job_history", "job_progress", "job_progress_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "worker_primary_jobs", "worker_primary_job_history", "worker_pause_state", "worker_pause_history", "notes", "note_revisions", "events", "receipts"} {
		rows, err := a.store.DB().Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		values := [][]any{}
		for rows.Next() {
			row := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range row {
				pointers[i] = &row[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			values = append(values, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		out[table] = sha256.Sum256(encoded)
	}
	return out
}
