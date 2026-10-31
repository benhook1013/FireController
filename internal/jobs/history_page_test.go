package jobs_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestJobHistoryStoredStateRefusalAndLegacyShape(t *testing.T) {
	for _, damage := range []string{"syntax", "duplicate", "unicode", "null", "missing", "unknown", "wrong-job", "oversized", "invalid-progress", "legacy"} {
		t.Run(damage, func(t *testing.T) {
			a, project, token, _, base := progressFixture(t)
			jobID := strings.TrimPrefix(base, "/api/v1/projects/"+project+"/jobs/")
			var original string
			if err := a.store.DB().QueryRow(`SELECT state_json FROM job_history WHERE project_id=? AND job_id=? AND revision=1`, project, jobID).Scan(&original); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal([]byte(original), &fields) != nil {
				t.Fatal("fixture snapshot")
			}
			state := original
			switch damage {
			case "syntax":
				state = `{"title":`
			case "duplicate":
				state = `{"title":"duplicate",` + original[1:]
			case "unicode":
				state = strings.Replace(original, `"brief":`+string(fields["brief"]), `"brief":"\ud800"`, 1)
			case "null":
				fields["brief"] = json.RawMessage(`null`)
			case "missing":
				delete(fields, "worker_id")
			case "unknown":
				fields["unknown"] = json.RawMessage(`true`)
			case "wrong-job":
				fields["id"] = json.RawMessage(`"other-job"`)
			case "oversized":
				state = strings.Repeat(" ", (1<<20)+1) + original
			case "invalid-progress":
				fields["progress"] = json.RawMessage(`{"summary":"x","next":null,"blocker":"","evidence":[]}`)
			case "legacy":
				delete(fields, "progress")
			}
			if state == original {
				encoded, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				state = string(encoded)
			}
			if _, err := a.store.DB().Exec(`UPDATE job_history SET state_json=? WHERE project_id=? AND job_id=? AND revision=1`, state, project, jobID); err != nil {
				t.Fatal(err)
			}
			response := a.call(http.MethodGet, base+"/history", token, "", nil)
			if damage == "legacy" {
				if response.Code != 200 {
					t.Fatalf("supported legacy snapshot refused: %d %s", response.Code, response.Body.String())
				}
				var page struct {
					Items []struct {
						State map[string]json.RawMessage `json:"state"`
					} `json:"items"`
				}
				decodeResponse(t, response, &page)
				if len(page.Items) != 1 {
					t.Fatal("legacy history lost")
				}
				if _, ok := page.Items[0].State["progress"]; ok {
					t.Fatal("legacy shape changed")
				}
			} else {
				var failure struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				decodeResponse(t, response, &failure)
				if response.Code != 500 || failure.Error.Code != "storage_unavailable" {
					t.Fatalf("damaged snapshot response: status=%d code=%s", response.Code, failure.Error.Code)
				}
			}
			var after string
			if err := a.store.DB().QueryRow(`SELECT state_json FROM job_history WHERE project_id=? AND job_id=? AND revision=1`, project, jobID).Scan(&after); err != nil || after != state {
				t.Fatal("history read mutated state", err)
			}
		})
	}
}
