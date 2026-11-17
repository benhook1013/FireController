package jobs_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func createCheckpointTestJob(t *testing.T, a *testAPI, project, token string) jobValue {
	t.Helper()
	created := a.call(http.MethodPost, "/api/v1/projects/"+project+"/jobs", token, "checkpoint-job-create", map[string]any{
		"title": "Checkpoint test", "brief": "Current brief must remain unchanged", "status": "active",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create job status=%d body=%s", created.Code, created.Body.String())
	}
	var receipt receipt
	decodeResponse(t, created, &receipt)
	response := a.call(http.MethodGet, "/api/v1/projects/"+project+"/jobs/"+receipt.ID, token, "", nil)
	var job jobValue
	decodeResponse(t, response, &job)
	return job
}

func (a *testAPI) callWorker(method, path, token, worker, key string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-FC-Instance", a.store.InstanceID())
	request.Header.Set("X-FC-Worker", worker)
	request.Header.Set("Idempotency-Key", key)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	a.mux.ServeHTTP(response, request)
	return response
}

type checkpointAPIValue struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id"`
	JobRevision int64    `json:"job_revision"`
	Sequence    int64    `json:"sequence"`
	ActorID     string   `json:"actor_id"`
	WorkerID    string   `json:"worker_id"`
	Done        string   `json:"done"`
	Next        string   `json:"next"`
	Blocker     string   `json:"blocker"`
	Evidence    []string `json:"evidence"`
	CreatedAt   string   `json:"created_at"`
}
