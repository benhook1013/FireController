package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewCheckpointMCPActualPublicReplayRecoveryAndRead(t *testing.T) {
	f, handler, evidence := reviewControlsFixture(t)
	var drop atomic.Bool
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.RawQuery != "" || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != f.sender {
			t.Error("SDK checkpoint lost bound scope")
		}
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/publications") {
			posts.Add(1)
			if r.Header.Get("Prefer") != "wait=0" {
				t.Error("wait preference lost")
			}
			if drop.Swap(false) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				if rec.Code != 202 {
					t.Error("checkpoint notcommitted", rec.Code, rec.Body.String())
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, srv.URL), "chosen")
	var receipt client.ReviewControlReceipt
	decodeResult(t, call(t, session, "review_finding_decide", map[string]any{"evidence_id": evidence, "idempotency_key": "reject-native", "expected_revision": 0, "finding": 0, "disposition": "rejected", "reason": "Original native source rejected"}), &receipt)
	decodeResult(t, call(t, session, "review_source_finalize", map[string]any{"evidence_id": evidence, "idempotency_key": "freeze-native", "expected_revision": 1}), &receipt)
	args := map[string]any{"evidence_id": evidence, "idempotency_key": "checkpoint-once", "expected_revision": 2, "wait_seconds": 0}
	provider := f.providers.Load()
	drop.Store(true)
	diagnosticResult(t, call(t, session, "review_checkpoint_publish", args), "connection_failed", true)
	if posts.Load() != 1 {
		t.Fatal("lost post implicitlyretried")
	}
	var queued, replay client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_checkpoint_publish", args), &queued)
	decodeResult(t, call(t, session, "review_checkpoint_publish", args), &replay)
	if queued != replay || queued.State != "queued" {
		t.Fatal("exactkey changed action", queued, replay)
	}
	var operation client.ReviewCheckpointPublication
	decodeResult(t, call(t, session, "review_checkpoint_read", map[string]any{"publication_id": queued.ID}), &operation)
	if operation.ProjectID != f.project || operation.EvidenceID != evidence || operation.SourceRevision != 2 || operation.Counts != (client.ReviewCounts{Found: 1}) || operation.CommentID != 0 {
		t.Fatal("stored publication source/confirmation invalid", operation)
	}
	decodeResult(t, call(t, session, "review_checkpoint_recover", map[string]any{"publication_id": queued.ID, "idempotency_key": "observe-only", "expected_revision": 1, "retry_unposted": false, "wait_seconds": 0}), &replay)
	if replay.ID != queued.ID || f.providers.Load() != provider {
		t.Fatal("local queued recovery fabricated provider work", replay)
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_checkpoint_publications`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate public checkpoint", count, err)
	}
}

func TestReviewCheckpointMCPDiscoveryAndInvalidBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer srv.Close()
	binding := localContext()
	binding.ServerURL = srv.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-checkpoint-token"), "chosen")
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"review_checkpoint_publish": 4, "review_checkpoint_read": 1, "review_checkpoint_recover": 5}
	reads, writes := 0, 0
	for _, tool := range tools.Tools {
		if tool.Annotations.ReadOnlyHint {
			reads++
		} else {
			writes++
		}
		n, ok := want[tool.Name]
		if !ok {
			continue
		}
		delete(want, tool.Name)
		var shape struct {
			Properties map[string]any
			Required   []string
			Additional bool `json:"additionalProperties"`
		}
		raw, _ := json.Marshal(tool.InputSchema)
		write := tool.Name != "review_checkpoint_read"
		required := 3
		if !write {
			required = 1
		}
		if json.Unmarshal(raw, &shape) != nil || len(shape.Properties) != n || len(shape.Required) != required || shape.Additional || tool.Annotations.ReadOnlyHint == write || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint != write || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint {
			t.Fatal("incorrect checkpoint schema/effects", tool.Name)
		}
	}
	if len(want) != 0 || len(tools.Tools) != 45 || reads != 29 || writes != 16 {
		t.Fatal("discovery totals wrong", want, reads, writes)
	}
	id := "review_publication_" + strings.Repeat("a", 32)
	evidence := "review_evidence_" + strings.Repeat("a", 32)
	for name, args := range map[string]map[string]any{"review_checkpoint_publish": {"evidence_id": evidence, "expected_revision": 0, "idempotency_key": "key"}, "review_checkpoint_read": {"publication_id": id}, "review_checkpoint_recover": {"publication_id": id, "expected_revision": 1, "idempotency_key": "key"}} {
		bad := func(v any) { t.Helper(); diagnosticResult(t, call(t, session, name, v), "invalid_arguments", false) }
		for field := range args {
			copy := map[string]any{}
			for k, v := range args {
				if k != field {
					copy[k] = v
				}
			}
			bad(copy)
			copy[field] = nil
			bad(copy)
		}
		for _, field := range []string{"profile", "project_id", "worker_id", "extra"} {
			copy := map[string]any{}
			for k, v := range args {
				copy[k] = v
			}
			copy[field] = "foreign"
			bad(copy)
		}
		if name != "review_checkpoint_read" {
			for _, v := range []any{nil, -1, 61, 1.5, "2"} {
				copy := map[string]any{}
				for k, x := range args {
					copy[k] = x
				}
				copy["wait_seconds"] = v
				bad(copy)
			}
		}
		if name == "review_checkpoint_recover" {
			for _, v := range []any{nil, 1, "true"} {
				copy := map[string]any{}
				for k, x := range args {
					copy[k] = x
				}
				copy["retry_unposted"] = v
				bad(copy)
			}
		}
		data, _ := json.Marshal(args)
		for field := range args {
			bad(json.RawMessage(strings.Replace(string(data), `"`+field+`":`, `"`+strings.ToUpper(field)+`":`, 1)))
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid checkpoint args accessed credentials/HTTP")
	}
}
