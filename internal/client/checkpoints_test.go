package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func checkpointRecord() client.Checkpoint {
	return client.Checkpoint{ID: "checkpoint-one", ProjectID: "project-one", JobID: "job-one", JobRevision: 2, Sequence: 7, ActorID: "principal-one", WorkerID: "worker-one", Done: "  finished\n", Next: "continue\twork", Blocker: "", Evidence: []string{"proof-one"}, CreatedAt: "2026-10-15T00:00:00Z"}
}

func TestCheckpointClientScopedReceiptsLatestAndExplicitArchive(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		requests.Add(1)
		if r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("checkpoint binding lost")
		}
		if r.Method == "POST" {
			var request client.AppendCheckpointRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.Done != "  exact done\n" || request.Next != "next\tstep" || request.Evidence == nil || len(request.Evidence) != 0 || r.Header.Get("Idempotency-Key") != "explicit" {
				t.Error("checkpoint text/key/evidence canonicalization changed")
			}
			write(w, 201, client.CheckpointReceipt{ID: "checkpoint-one", Sequence: 7, JobRevision: 2, Outcome: "created"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/latest") {
			write(w, 200, client.LatestCheckpointResult{JobID: "job-one", JobRevision: 3, Checkpoint: ptrCheckpoint(checkpointRecord())})
			return
		}
		if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("cursor") != "explicit-cursor" {
			t.Error("archive page options changed")
		}
		write(w, 200, client.Page[client.Checkpoint]{Items: []client.Checkpoint{checkpointRecord()}, Cursor: "next-cursor", HasMore: true})
	}))
	defer server.Close()
	c := newClient(t, server)
	receipt, err := c.AppendCheckpoint(context.Background(), "explicit", "job-one", client.AppendCheckpointRequest{ExpectedJobRevision: 2, Done: "  exact done\n", Next: "next\tstep"})
	if err != nil || receipt.JobRevision != 2 || receipt.Sequence != 7 {
		t.Fatalf("append: %+v %v", receipt, err)
	}
	latest, err := c.LatestCheckpoint(context.Background(), "job-one")
	if err != nil || latest.Checkpoint == nil || latest.JobRevision != 3 || latest.Checkpoint.JobRevision != 2 || latest.Checkpoint.Done != "  finished\n" {
		t.Fatalf("latest captured/current revision: %+v %v", latest, err)
	}
	page, err := c.CheckpointHistory(context.Background(), "job-one", client.PageOptions{Limit: 1, Cursor: "explicit-cursor"})
	if err != nil || len(page.Items) != 1 || !page.HasMore || page.Cursor != "next-cursor" {
		t.Fatalf("archive completeness: %+v %v", page, err)
	}
	if requests.Load() != 3 {
		t.Fatal("automatic archive/retry request")
	}
}

func ptrCheckpoint(value client.Checkpoint) *client.Checkpoint { return &value }

func TestCheckpointClientNullLatestAndStrictResponses(t *testing.T) {
	valid, _ := json.Marshal(client.LatestCheckpointResult{JobID: "job-one", JobRevision: 3, Checkpoint: ptrCheckpoint(checkpointRecord())})
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"none", `{"job_id":"job-one","job_revision":3,"checkpoint":null}`, true},
		{"null-job", `{"job_id":null,"job_revision":3,"checkpoint":null}`, false},
		{"null-revision", `{"job_id":"job-one","job_revision":null,"checkpoint":null}`, false},
		{"oversized-latest", strings.Repeat(" ", 128<<10) + string(valid), false},
		{"malformed-nullable", `{"job_id":"job-one","job_revision":3,"checkpoint":[]}`, false},
		{"outer-unknown", `{"job_id":"job-one","job_revision":3,"checkpoint":null,"other":true}`, false},
		{"missing-nullable", `{"job_id":"job-one","job_revision":3}`, false},
		{"missing-required", strings.Replace(string(valid), `"blocker":"",`, "", 1), false},
		{"null-evidence", strings.Replace(string(valid), `["proof-one"]`, `null`, 1), false},
		{"wrong-project", strings.Replace(string(valid), `project-one`, `project-two`, 1), false},
		{"wrong-job", strings.Replace(string(valid), `"job_id":"job-one"`, `"job_id":"job-two"`, 1), false},
		{"future-capture", strings.Replace(string(valid), `"job_revision":2`, `"job_revision":4`, 1), false},
		{"unknown", strings.Replace(string(valid), `"blocker":""`, `"blocker":"","unknown":true`, 1), false},
		{"control", strings.Replace(string(valid), `finished\n`, `finished\u0001`, 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			c := newClient(t, server)
			value, err := c.LatestCheckpoint(context.Background(), "job-one")
			if test.valid {
				if err != nil || value.Checkpoint != nil {
					t.Fatalf("null latest: %+v %v", value, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
	for _, items := range [][]client.Checkpoint{{checkpointRecord(), checkpointRecord()}, {func() client.Checkpoint { x := checkpointRecord(); x.Sequence = 6; return x }(), checkpointRecord()}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			write(w, 200, client.Page[client.Checkpoint]{Items: items})
		}))
		c := newClient(t, server)
		_, err := c.CheckpointHistory(context.Background(), "job-one", client.PageOptions{})
		checkError(t, err, "invalid_response")
		c.Close()
		server.Close()
	}
}

func TestCheckpointClientCanceledDispatchedWriteIsUnknownWithoutRetry(t *testing.T) {
	started := make(chan struct{})
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		writes.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer server.Close()
	c := newClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		_, err := c.AppendCheckpoint(ctx, "explicit", "job-one", client.AppendCheckpointRequest{ExpectedJobRevision: 1, Done: "done", Next: "next"})
		completed <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("checkpoint write did not dispatch within test bound")
	}
	cancel()
	select {
	case err := <-completed:
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "canceled" || !failure.OutcomeUnknown || writes.Load() != 1 {
			t.Fatalf("canceled checkpoint write: %v, writes=%d", err, writes.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("checkpoint cancellation did not reach HTTP promptly")
	}
}

func TestCheckpointClientValidationAndUnknownDispatchedReceipt(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		requests.Add(1)
		write(w, 201, map[string]any{"id": "checkpoint-one", "sequence": 1, "job_revision": 99, "outcome": "created"})
	}))
	defer server.Close()
	c := newClient(t, server)
	base := client.AppendCheckpointRequest{ExpectedJobRevision: 1, Done: "done", Next: "next"}
	for _, change := range []func(*client.AppendCheckpointRequest){
		func(v *client.AppendCheckpointRequest) { v.ExpectedJobRevision = 0 }, func(v *client.AppendCheckpointRequest) { v.Done = " " }, func(v *client.AppendCheckpointRequest) { v.Next = "bad\x7f" }, func(v *client.AppendCheckpointRequest) { v.Done = strings.Repeat("é", 4097) }, func(v *client.AppendCheckpointRequest) { v.Blocker = strings.Repeat("x", 4097) }, func(v *client.AppendCheckpointRequest) { v.Evidence = []string{" "} }, func(v *client.AppendCheckpointRequest) { v.Evidence = make([]string, 11) }, func(v *client.AppendCheckpointRequest) {
			v.Done = strings.Repeat("x", 8192)
			v.Next = strings.Repeat("x", 8192)
			v.Blocker = "x"
		},
	} {
		request := base
		change(&request)
		if _, err := c.AppendCheckpoint(context.Background(), "explicit", "job-one", request); err == nil {
			t.Fatal("invalid checkpoint accepted")
		}
	}
	if _, err := c.CheckpointHistory(context.Background(), "job-one", client.PageOptions{Limit: 101}); err == nil {
		t.Fatal("invalid page accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("invalid checkpoint made HTTP request")
	}
	_, err := c.AppendCheckpoint(context.Background(), "explicit", "job-one", base)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || failure.Code != "invalid_response" || requests.Load() != 1 {
		t.Fatalf("unknown receipt/retry: %v %d", err, requests.Load())
	}
}
