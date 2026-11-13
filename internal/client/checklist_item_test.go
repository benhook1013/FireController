package client_test

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestChecklistItemClientBoundedRequestAndReceipt(t *testing.T) {
	var writes int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		writes++
		if r.Method != "PATCH" || r.URL.Path != "/api/v1/projects/project-one/jobs/job-one/checklist/items/item_1" || r.Header.Get("Idempotency-Key") != "selected-key" {
			t.Error("incorrect item write", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"expected_revision":7,"done":false}` {
			t.Error("item write included unrelated content or dropped explicit false", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.ChecklistReceipt{ID: "job-one", Revision: 8, JobRevision: 42, Outcome: "updated"})
	}))
	defer s.Close()
	c := newClient(t, s)
	got, err := c.UpdateJobChecklistItem(t.Context(), "selected-key", "job-one", "item_1", client.UpdateChecklistItemRequest{ExpectedRevision: 7, Done: false})
	if err != nil || got.JobRevision != 42 || writes != 1 {
		t.Fatal("item request depended on job revision or repeated I/O", got, err, writes)
	}
}

func TestChecklistItemClientLocalValidationAndStrictReceipt(t *testing.T) {
	for _, revision := range []int64{-1, 0, math.MaxInt64} {
		if client.ValidateChecklistItemRequest(client.UpdateChecklistItemRequest{ExpectedRevision: revision}) == nil {
			t.Fatal("unwritable revision accepted", revision)
		}
	}
	for _, id := range []string{"", "has space", "é", "../one", "a/b", "a%20b"} {
		if client.ValidChecklistItemID(id) {
			t.Fatal("invalid item ID", id)
		}
	}
	for _, body := range []string{`{"id":"other","revision":2,"job_revision":1,"outcome":"updated"}`, `{"id":"job-one","revision":2,"job_revision":0,"outcome":"updated"}`, `{"id":"job-one","revision":1,"job_revision":1,"outcome":"updated"}`, `{"id":"job-one","revision":2,"job_revision":1,"outcome":"unchanged"}`, `{"id":"job-one","revision":2,"job_revision":1,"outcome":"updated","body":"unexpected"}`, `{"id":"job-one","revision":2,"outcome":"updated"}`} {
		c, calls := progressResponseClient(t, body)
		_, err := c.UpdateJobChecklistItem(t.Context(), "key", "job-one", "one", client.UpdateChecklistItemRequest{ExpectedRevision: 1, Done: true})
		if err == nil || calls.Load() != 1 {
			t.Fatal("malformed receipt accepted or retried", body, err, calls.Load())
		}
	}
	c, calls := progressResponseClient(t, `{}`)
	for _, request := range []struct {
		key, job, item string
		revision       int64
	}{{"", "job-one", "one", 1}, {"key", "../job", "one", 1}, {"key", "job-one", "one/two", 1}, {"key", "job-one", "one", 0}} {
		if _, err := c.UpdateJobChecklistItem(t.Context(), request.key, request.job, request.item, client.UpdateChecklistItemRequest{ExpectedRevision: request.revision}); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("local validation reached server")
	}
}
