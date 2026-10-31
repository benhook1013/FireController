package client_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReviewOperationWaitNegotiatedFullCompactAndLegacy(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, paused := range []bool{false, true} {
			for _, legacy := range []bool{false, true} {
				t.Run(strings.Join([]string{boolName(compact), boolName(paused), boolName(legacy)}, "/"), func(t *testing.T) {
					op := sampleReviewOperation()
					op.Channel = "hosted"
					op.State = "queued"
					op.AttemptID = ""
					op.Reason = ""
					wait := &client.ReviewOperationWait{BaselineAttemptID: "review_attempt_" + strings.Repeat("b", 32), RetryNotBefore: "2026-01-01T00:10:00Z", Paused: paused}
					if !legacy {
						op.Wait = wait
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/instance" {
							handshake(w)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						if r.Header.Get("X-FC-Operation-Wait") != "1" {
							t.Error("wait extension not negotiated")
						}
						if compact {
							_ = json.NewEncoder(w).Encode(client.ReviewOperationStatus{ID: op.ID, PullNumber: op.PullNumber, Channel: op.Channel, State: op.State, Revision: op.Revision, Stage: "queued", Reason: "", AttemptID: "", UpdatedAt: op.UpdatedAt, NextAction: "wait", Wait: op.Wait})
						} else {
							_ = json.NewEncoder(w).Encode(op)
						}
					}))
					defer server.Close()
					c := newClient(t, server)
					if compact {
						v, err := c.ReviewOperationStatus(t.Context(), op.ID)
						if err != nil || (v.Wait == nil) != legacy || v.Wait != nil && *v.Wait != *wait {
							t.Fatal(v, err)
						}
					} else {
						v, err := c.ReviewOperation(t.Context(), op.ID)
						if err != nil || (v.Wait == nil) != legacy || v.Wait != nil && *v.Wait != *wait {
							t.Fatal(v, err)
						}
					}
				})
			}
		}
	}
}
func boolName(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func TestReviewOperationControlBoundSingleShotAndMalformed(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	var requests atomic.Int64
	for _, action := range []string{"pause", "resume", "cancel"} {
		t.Run(action, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				requests.Add(1)
				body, _ := io.ReadAll(r.Body)
				want := `{"expected_revision":3,"action":"` + action + `"}`
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/operations/"+id+"/control" || r.Header.Get("Idempotency-Key") != "same-key" || r.Header.Get("X-FC-Worker") != "worker-one" || string(body) != want {
					t.Error("bound exact control changed", r.URL, string(body))
				}
				state := "queued"
				if action == "cancel" {
					state = "cancelled"
				}
				_ = json.NewEncoder(w).Encode(client.ReviewOperationControlReceipt{ID: id, State: state, Revision: 4, Paused: action == "pause", RetryNotBefore: "2026-01-01T00:10:00Z"})
			}))
			defer server.Close()
			c := newClient(t, server)
			v, err := c.ControlReviewOperation(t.Context(), "same-key", id, client.ReviewOperationControl{ExpectedRevision: 3, Action: action})
			if err != nil || v.Revision != 4 || v.ID != id {
				t.Fatal(v, err)
			}
			before := requests.Load()
			for _, bad := range []client.ReviewOperationControl{{ExpectedRevision: 0, Action: action}, {ExpectedRevision: 3, Action: "stop"}} {
				_, err := c.ControlReviewOperation(t.Context(), "same-key", id, bad)
				if err == nil {
					t.Fatal("bad control accepted")
				}
			}
			_, err = c.ControlReviewOperation(t.Context(), "same-key", "bad", client.ReviewOperationControl{ExpectedRevision: 3, Action: action})
			if err == nil || requests.Load() != before {
				t.Fatal("malformed contacted service")
			}
		})
	}
}

func TestReviewOperationWaitRejectsMalformedFacts(t *testing.T) {
	for _, bad := range []string{`null`, `{"paused":null}`, `{"paused":true,"retry_not_before":"2026-01-01T00:10:00Z"}`, `{"paused":false,"baseline_attempt_id":"review_attempt_` + strings.Repeat("a", 32) + `"}`, `{"paused":true,"private":"secret"}`, `{"paused":false,"paused":true}`} {
		t.Run(bad, func(t *testing.T) {
			op := sampleReviewOperation()
			op.Channel = "hosted"
			op.State = "queued"
			op.AttemptID = ""
			raw, _ := json.Marshal(op)
			raw = append(raw[:len(raw)-1], []byte(`,"wait":`+bad+`}`)...)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(raw)
			}))
			defer server.Close()
			_, err := newClient(t, server).ReviewOperation(t.Context(), op.ID)
			if err == nil {
				t.Fatal("malformed wait accepted")
			}
		})
	}
}

func TestReviewOperationControlActualWriterReadAndProjectIsolation(t *testing.T) {
	f, _ := executionFixture(t)
	for token, permissions := range map[string][]string{"synthetic-writer": {"read", "write"}, "synthetic-reader": {"read"}} {
		sum := sha256.Sum256([]byte(token))
		f.call(t, "POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, f.sender, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": permissions})
	}
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	writer := f.newClient(t, "synthetic-writer", f.sender, srv.URL)
	peer := f.newClient(t, "synthetic-writer", f.recipient, srv.URL)
	reader := f.newClient(t, "synthetic-reader", f.sender, srv.URL)
	queued, err := queuedReviewRequest(t, writer, t.Context(), "request", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	before, err := reader.ReviewOperation(t.Context(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reader.ControlReviewOperation(t.Context(), "reader-pause", queued.ID, client.ReviewOperationControl{ExpectedRevision: 1, Action: "pause"})
	checkError(t, err, "forbidden")
	paused, err := peer.ControlReviewOperation(t.Context(), "peer-pause", queued.ID, client.ReviewOperationControl{ExpectedRevision: 1, Action: "pause"})
	if err != nil || !paused.Paused {
		t.Fatal(paused, err)
	}
	after, err := reader.ReviewOperation(t.Context(), queued.ID)
	if err != nil || after.ActorID != before.ActorID || after.WorkerID != f.sender || after.Wait == nil || !after.Wait.Paused {
		t.Fatal(after, err)
	}
	foreign := mutationFixture{store: f.store, admin: f.admin, tokenFile: f.tokenFile}
	foreign.project = f.call(t, "POST", "/api/v1/projects", f.admin, "", map[string]any{"name": "Other", "repository": "other/repository"})["id"].(string)
	otherWorker := f.call(t, "POST", "/api/v1/projects/"+foreign.project+"/workers", f.admin, "", map[string]any{"name": "Other"})["id"].(string)
	_, err = foreign.newClient(t, f.admin, otherWorker, srv.URL).ControlReviewOperation(t.Context(), "foreign-pause", queued.ID, client.ReviewOperationControl{ExpectedRevision: 2, Action: "pause"})
	checkError(t, err, "not_found")
	_, err = peer.ControlReviewOperation(t.Context(), "cancel", queued.ID, client.ReviewOperationControl{ExpectedRevision: 2, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = peer.ControlReviewOperation(t.Context(), "cannot-resume", queued.ID, client.ReviewOperationControl{ExpectedRevision: 3, Action: "resume"})
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "review_operation_started" || !strings.Contains(failure.Action, "No provider cancellation occurred") || !strings.Contains(failure.Action, "held target_changed") || !strings.Contains(failure.Action, "Do not submit a replacement for posting, active or uncertain work") {
		t.Fatal(err)
	}
	done, err := reader.ReviewOperation(t.Context(), queued.ID)
	if err != nil || done.State != "cancelled" || done.Wait != nil {
		t.Fatal(done, err)
	}
}
