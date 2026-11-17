package client_test

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

const checkpointID = "review_publication_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func sampleCheckpoint() client.ReviewCheckpointPublication {
	return client.ReviewCheckpointPublication{ID: checkpointID, ProjectID: "project-one", EvidenceID: controlEvidenceID, State: "published", Revision: 4, SourceRevision: 2, Anchor: sampleControlEvidence().Anchor, Counts: client.ReviewCounts{Found: 2, Accepted: 1, Routed: 1}, CreatedAt: "2026-10-15T00:00:00.123Z", UpdatedAt: "2026-10-15T00:00:01Z", PublishedAt: "2026-10-15T00:00:00Z", CommentID: 7, CommentURL: "https://github.com/renamed/repository/pull/2#issuecomment-7", ProviderAuthorID: 9, ProviderAuthorKind: "User"}
}

// Controlled wire responses isolate transport/shape handling from core execution.
func TestReviewCheckpointClientFlatWaitStrictAndUnknown(t *testing.T) {
	var response atomic.Value
	response.Store(`{}`)
	var status atomic.Int64
	status.Store(200)
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.URL.RawQuery != "" || r.Header.Get("X-FC-Instance") == "" || r.Header.Get("X-FC-Worker") == "" {
			t.Error("binding lost")
		}
		if r.Method == "POST" {
			posts.Add(1)
			if r.Header.Get("Idempotency-Key") != "checkpoint-key" || r.Header.Get("Prefer") != "wait=0" {
				t.Error("key/wait changed")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(response.Load().(string)))
	}))
	defer srv.Close()
	c := newClient(t, srv)
	request := client.ReviewCheckpointRequest{EvidenceID: controlEvidenceID, ExpectedRevision: math.MaxInt64}
	valid, _ := json.Marshal(sampleCheckpoint())
	response.Store(string(valid))
	got, err := c.RequestReviewCheckpoint(t.Context(), "checkpoint-key", request, 0)
	if err != nil || got.Operation == nil || got.Receipt != nil {
		t.Fatal(got, err)
	}
	flat, _ := json.Marshal(got)
	if strings.Contains(string(flat), "Operation") || len(flat) > 16<<10 {
		t.Fatal("wrapped or large", string(flat))
	}
	// Provider confirmation identity is authoritative; host clocks are independent.
	for _, at := range []string{"2026-10-14T23:50:00Z", "2026-10-15T00:10:00Z"} {
		skewed := sampleCheckpoint()
		skewed.PublishedAt = at
		skewed.ProviderAuthorKind = "Organization"
		data, _ := json.Marshal(skewed)
		response.Store(string(data))
		if _, err = c.ReviewCheckpointPublication(t.Context(), checkpointID); err != nil {
			t.Fatal("provider/local clock skew rejected", at, err)
		}
		if _, err = c.RequestReviewCheckpoint(t.Context(), "checkpoint-key", request, 0); err != nil {
			t.Fatal("skewed provider confirmation rejected", at, err)
		}
	}
	for _, state := range []string{"held", "uncertain", "queued", "preparing", "posting", "recover_queued", "recovering", "cancelled"} {
		v := sampleCheckpoint()
		v.State = state
		v.CommentID = 0
		v.CommentURL = ""
		v.PublishedAt = ""
		v.ProviderAuthorID = 0
		v.ProviderAuthorKind = ""
		body, _ := json.Marshal(v)
		response.Store(string(body))
		if _, err = c.ReviewCheckpointPublication(t.Context(), checkpointID); err != nil {
			t.Fatal(state, err)
		}
		if state == "cancelled" {
			result, err := c.RequestReviewCheckpoint(t.Context(), "checkpoint-key", request, 0)
			if err != nil || result.Operation == nil || result.Operation.State != "cancelled" {
				t.Fatal("cancelled terminal replay", result, err)
			}
		}
	}
	for _, edit := range []func(map[string]any){
		func(v map[string]any) { v["project_id"] = "foreign" }, func(v map[string]any) { v["comment_id"] = 0 }, func(v map[string]any) { v["comment_url"] = "https://github.com/owner/repo/pull/3#issuecomment-7" }, func(v map[string]any) { v["published_at"] = "2026-10-15T00:00:00+01:00" }, func(v map[string]any) { v["counts"].(map[string]any)["accepted"] = 2 }, func(v map[string]any) { v["state"] = "posting" }, func(v map[string]any) { v["provider_author_kind"] = "unknown" },
	} {
		var body map[string]any
		_ = json.Unmarshal(valid, &body)
		edit(body)
		data, _ := json.Marshal(body)
		response.Store(string(data))
		_, err = c.RequestReviewCheckpoint(t.Context(), "checkpoint-key", request, 0)
		checkError(t, err, "invalid_response")
		var diagnostic *client.Error
		if !errors.As(err, &diagnostic) || !diagnostic.OutcomeUnknown {
			t.Fatal("mutation uncertainty lost", err)
		}
	}
	var fields map[string]any
	_ = json.Unmarshal(valid, &fields)
	for field := range fields {
		for _, remove := range []bool{false, true} {
			var body map[string]any
			_ = json.Unmarshal(valid, &body)
			if remove {
				delete(body, field)
			} else {
				body[field] = nil
			}
			data, _ := json.Marshal(body)
			response.Store(string(data))
			_, err = c.ReviewCheckpointPublication(t.Context(), checkpointID)
			checkError(t, err, "invalid_response")
		}
	}
	status.Store(202)
	response.Store(`{"id":"` + checkpointID + `","state":"queued","revision":7}`)
	recovered, err := c.RecoverReviewCheckpoint(t.Context(), "checkpoint-key", checkpointID, client.ReviewCheckpointRecovery{ExpectedRevision: 6, RetryUnposted: true}, 0)
	if err != nil || recovered.Receipt == nil || recovered.Receipt.Revision != 7 {
		t.Fatal("recovery receipt need not start at1", recovered, err)
	}
	status.Store(409)
	response.Store(`{"error":{"code":"permission_denied","message":"No longer authorized.","details":{"outcome_unknown":true}}}`)
	before := posts.Load()
	_, err = c.RequestReviewCheckpoint(t.Context(), "checkpoint-key", request, 0)
	var diagnostic *client.Error
	if !errors.As(err, &diagnostic) || !diagnostic.OutcomeUnknown || posts.Load() != before+1 {
		t.Fatal("afterenqueue refusal notunknown/onepost", err)
	}
}
func TestReviewCheckpointClientLostResponseAndLocalInvalid(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		posts.Add(1)
		conn, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	c := newClient(t, srv)
	_, err := c.RequestReviewCheckpoint(t.Context(), "same-key", client.ReviewCheckpointRequest{EvidenceID: controlEvidenceID}, 2)
	var diagnostic *client.Error
	if !errors.As(err, &diagnostic) || !diagnostic.OutcomeUnknown || posts.Load() != 1 {
		t.Fatal("lost response retried", err, posts.Load())
	}
	var absent *client.Client
	for _, r := range []client.ReviewCheckpointRequest{{EvidenceID: "bad"}, {EvidenceID: controlEvidenceID, ExpectedRevision: -1}} {
		_, e := absent.RequestReviewCheckpoint(t.Context(), "key", r, 2)
		checkError(t, e, "validation_failed")
	}
	for _, r := range []client.ReviewCheckpointRecovery{{}, {ExpectedRevision: math.MaxInt64}} {
		_, e := absent.RecoverReviewCheckpoint(t.Context(), "key", checkpointID, r, 2)
		checkError(t, e, "validation_failed")
	}
	_, err = absent.RequestReviewCheckpoint(t.Context(), "", client.ReviewCheckpointRequest{EvidenceID: controlEvidenceID}, 2)
	checkError(t, err, "validation_failed")
	_, err = absent.RecoverReviewCheckpoint(t.Context(), "key", checkpointID, client.ReviewCheckpointRecovery{ExpectedRevision: 1}, 901)
	checkError(t, err, "validation_failed")
}
