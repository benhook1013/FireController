package cli_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

const cliSelectionID = "review_selection_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestReviewRunCLIInvalidBeforeCredentials(t *testing.T) {
	base := []string{"reviews", "start", "--channel", "cli", "--key", "selection-once"}
	cases := [][]string{{"reviews", "start"}, {"reviews", "start", "--channel", "cli"}, {"reviews", "start", "--channel", "other", "--key", "key"}, {"reviews", "run-read"}, {"reviews", "run-read", "--selection-id", "bad"}, {"reviews", "run-read", "--selection-id", cliSelectionID, "--key", "forbidden"}}
	for _, suffix := range [][]string{{"--expect-pr", "0"}, {"--expect-pr", "02"}, {"--expect-pr", "2147483648"}, {"--expected-configuration-revision", "0"}, {"--expected-configuration-revision", "+1"}, {"--expected-configuration-revision", "9223372036854775807"}, {"--wait-seconds", "-1"}, {"--wait-seconds", "901"}, {"--wait-seconds", "00"}, {"--wait-seconds", "0", "--wait-seconds", "1"}, {"--channel", "hosted"}, {"--worker", "other"}, {"--head", strings.Repeat("a", 40)}, {"--snapshot", "snapshot"}, {"unexpected"}} {
		cases = append(cases, append(append([]string{}, base...), suffix...))
	}
	for _, args := range cases {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing/selection-profiles"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("invalid selection reached credentials", args, code, stderr)
		}
	}
}

func TestReviewRunCLIActualHTTPQueuedReplayScopeAndLostOutput(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	otherProject := f.project(t, "selection-other")
	otherWorker := f.worker(t, otherProject, "Builder")
	colleague := f.worker(t, project, "Colleague")
	var posts atomic.Int64
	var calls atomic.Int64
	var drop atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == "POST" {
			posts.Add(1)
		}
		if r.Method == "POST" && drop.Swap(false) {
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, r)
			if rec.Code != 202 {
				t.Errorf("lost selection did not commit: %d", rec.Code)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}, "colleague": {project, colleague}, "other": {otherProject, otherWorker}})
	args := []string{"reviews", "start", "--channel", "cli", "--key", "selection-once", "--expect-pr", "2", "--expected-configuration-revision", "1", "--wait-seconds", "0"}
	provider := f.resolverHit.Load()
	drop.Store(true)
	code, out, stderr := mutationInvoke(t, profiles, "builder", args...)
	if code == 0 || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || posts.Load() != 1 {
		t.Fatal("lost selection retried or hid uncertainty", code, stderr)
	}
	counts := func() [3]int64 {
		t.Helper()
		var n [3]int64
		for i, table := range []string{"review_selections", "review_operations", "receipts"} {
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n[i]); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}
	committed := counts()
	if committed[0] != 1 || committed[1] != 0 {
		t.Fatal("acceptance created extra review work", committed)
	}
	before := calls.Load()
	receipt := mutationSuccess(t, profiles, "builder", args...)
	if receipt["state"] != "queued" || receipt["revision"] != float64(1) || calls.Load() != before+2 || counts() != committed {
		t.Fatal("exact selection replay changed effect", receipt)
	}
	id := receipt["id"].(string)
	code, out, stderr = mutationInvoke(t, profiles, "colleague", "reviews", "run-read", "--selection-id", id)
	var state client.ReviewRunState
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &state) != nil || state.Selection.ID != id || state.Selection.WorkerID != worker || state.Selection.ProjectID != project || state.Selection.State != "queued" || state.Operation != nil || counts() != committed {
		t.Fatal("stored selection read lost attribution or mutated", code, stderr)
	}
	code, out, stderr = mutationInvoke(t, profiles, "other", "reviews", "run-read", "--selection-id", id)
	if code == 0 || out != "" || strings.Contains(stderr, `"outcome_unknown":true`) {
		t.Fatal("foreign project read leaked selection", code, stderr)
	}
	// The public CLI must retain the original key even when stdout fails after
	// an accepted exact replay; the next invocation recovers that same receipt.
	var diagnostic strings.Builder
	full := append([]string{"--profiles-file", profiles, "--profile", "builder"}, args...)
	code = cli.RunContext(context.Background(), full, nil, selectionFailWriter{}, &diagnostic)
	if code != cli.ExitConfig || !strings.Contains(diagnostic.String(), `"outcome_unknown":true`) || !strings.Contains(diagnostic.String(), "exact same key") || counts() != committed {
		t.Fatal("output failure lost recovery", code, diagnostic.String())
	}
	recovered := mutationSuccess(t, profiles, "builder", args...)
	if encodeString(receipt) != encodeString(recovered) || counts() != committed {
		t.Fatal("output recovery reselected")
	}
	changed := append([]string{}, args...)
	for i := range changed {
		if changed[i] == "2" {
			changed[i] = "3"
			break
		}
	}
	code, out, stderr = mutationInvoke(t, profiles, "builder", changed...)
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "idempotency_conflict") || counts() != committed {
		t.Fatal("changed selection body replayed", code, stderr)
	}
	if f.resolverHit.Load() != provider {
		t.Fatal("CLI acceptance/read touched provider")
	}
}

type selectionFailWriter struct{}

func (selectionFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReviewRunCLIUsefulDefaultWaitAndPreservedHistoricalRead(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "selection-wire")
	worker := f.worker(t, project, "Builder")
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
			t.Error("lost context")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			posts.Add(1)
			if r.Header.Get("Prefer") != "wait=2" || r.Header.Get("Idempotency-Key") != "minimal-selection" {
				t.Error("default wait/key changed")
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["channel"] != "hosted" {
				t.Error("optional assertions were invented")
			}
			_ = json.NewEncoder(w).Encode(client.ReviewRunState{Selection: client.ReviewRunSelection{ID: cliSelectionID, ProjectID: project, ActorID: "principal_one", WorkerID: worker, Channel: "hosted", ConfigurationRevision: 1, PolicyRevision: 1, ExpectedPullNumber: 0, State: "empty", Reason: "queue_complete", Revision: 2, CreatedAt: "2026-10-18T01:00:00Z", UpdatedAt: "2026-10-18T00:59:00Z"}})
			return
		}
		// An old historical run request must still reach the original history route,
		// not be reinterpreted as a selection or mutation.
		if !strings.Contains(r.URL.Path, "/reviews/runs/review_run_") || r.Header.Get("Prefer") != "" {
			t.Error("historical run command was redirected")
		}
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"fixture historical run missing"},"request_id":"history"}`)
	}))
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	code, out, stderr := mutationInvoke(t, profiles, "builder", "reviews", "start", "--channel", "hosted", "--key", "minimal-selection")
	var result client.ReviewRunState
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &result) != nil || result.Selection.State != "empty" || posts.Load() != 1 || strings.Contains(out, `"Result"`) {
		t.Fatal("useful response expanded/polled", code, stderr)
	}
	code, _, stderr = mutationInvoke(t, profiles, "builder", "reviews", "run", "review_run_"+strings.Repeat("a", 32))
	if code == cli.ExitUsage || posts.Load() != 1 || !strings.Contains(stderr, "not_found") {
		t.Fatal("historical run parser changed", code, stderr)
	}
}
