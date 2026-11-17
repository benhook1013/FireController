package jobs_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestStoredHistoryValidationReturnsStorageUnavailable(t *testing.T) {
	for _, kind := range []string{"job-update", "worker-pause"} {
		t.Run(kind, func(t *testing.T) {
			a, project, token, worker, base := progressFixture(t)
			var route, query string
			var args []any
			if kind == "job-update" {
				if _, err := a.store.DB().Exec(`INSERT INTO job_updates(id,project_id,job_id,job_revision,actor_id,worker_id,body,evidence_json,created_at) VALUES(?,?,?,1,'legacy-actor','','original private update','[]','2026-01-01T00:00:00Z')`, "legacy-update", project, strings.TrimPrefix(base, "/api/v1/projects/"+project+"/jobs/")); err != nil {
					t.Fatal(err)
				}
				route = base + "/updates"
				query = `UPDATE job_updates SET evidence_json='null' WHERE project_id=?`
				args = []any{project}
			} else {
				route = pausePath(project, worker) + "/history"
				progressOK(t, a.callWorker("PUT", pausePath(project, worker), token, worker, "retained-pause", pauseRequest(0, true, "original private pause")), 200)
				// SQL accepts an oversized retained reason; the read contract refuses it.
				query = `UPDATE worker_pause_history SET reason=? WHERE project_id=? AND worker_id=?`
				args = []any{strings.Repeat("x", 1025), project, worker}
			}
			progressOK(t, a.call("GET", route, token, "", nil), 200)
			if _, e := a.store.DB().Exec(query, args...); e != nil {
				t.Fatal(e)
			}
			before := progressState(t, a)
			result := a.call("GET", route, token, "", nil)
			if result.Code != http.StatusInternalServerError {
				t.Fatalf("malformed state status=%d body=%s", result.Code, result.Body.String())
			}
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			decodeResponse(t, result, &failure)
			if failure.Error.Code != "storage_unavailable" {
				t.Fatalf("malformed state code=%s", failure.Error.Code)
			}
			if !reflect.DeepEqual(before, progressState(t, a)) {
				t.Fatal("read mutated retained data")
			}
		})
	}
}
