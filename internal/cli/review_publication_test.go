package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewCheckpointCLIActualRequestReplayReadRecovery(t *testing.T) {
	f, project, worker, evidence := routeCLIFixture(t)
	var drop atomic.Bool
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+f.admin || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != worker {
			t.Error("checkpoint binding changed")
		}
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/publications") {
			posts.Add(1)
			if r.Header.Get("Prefer") != "wait=0" {
				t.Error("immediate preference lost")
			}
			if drop.Swap(false) {
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, r)
				if rec.Code != 202 {
					t.Error("publication did not commit", rec.Code, rec.Body.String())
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	path := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	for _, step := range [][]string{{"decide", evidence, "--finding", "0", "--expect-revision", "0", "--disposition", "rejected", "--reason", "Original first rejected", "--key", "reject-first"}, {"decide", evidence, "--finding", "1", "--expect-revision", "1", "--disposition", "rejected", "--reason", "Original second rejected", "--key", "reject-second"}, {"finalize", evidence, "--expect-revision", "2", "--key", "freeze"}} {
		mutationSuccess(t, path, "builder", append([]string{"reviews"}, step...)...)
	}
	provider := f.resolverHit.Load()
	args := []string{"reviews", "publish", evidence, "--expect-revision", "3", "--key", "publish-once", "--wait-seconds", "0"}
	drop.Store(true)
	code, out, diagnostic := mutationInvoke(t, path, "builder", args...)
	if code != cli.ExitService || out != "" || !strings.Contains(diagnostic, `"outcome_unknown":true`) || posts.Load() != 1 {
		t.Fatal("unknown publication retried", code, out, diagnostic)
	}
	receipt := mutationSuccess(t, path, "builder", args...)
	replay := mutationSuccess(t, path, "builder", args...)
	if encodeString(receipt) != encodeString(replay) || len(receipt) != 3 {
		t.Fatal("key replay notcompact", receipt, replay)
	}
	id := receipt["id"].(string)
	code, out, diagnostic = mutationInvoke(t, path, "builder", "reviews", "publication", id)
	var current client.ReviewCheckpointPublication
	if code != 0 || diagnostic != "" || json.Unmarshal([]byte(out), &current) != nil || current.EvidenceID != evidence || current.State != "queued" || current.SourceRevision != 3 || current.Counts != (client.ReviewCounts{Found: 2}) {
		t.Fatal("local publication read changed source", code, out, diagnostic)
	}
	for _, retry := range []bool{false, true} {
		recovery := []string{"reviews", "publication-recover", id, "--expect-revision", "1", "--key", "recover-observe", "--wait-seconds=0"}
		if retry {
			recovery[len(recovery)-2] = "recover-unposted"
			recovery = append(recovery, "--retry-unposted")
		}
		r := mutationSuccess(t, path, "builder", recovery...)
		if r["id"] != id {
			t.Fatal("recovery replaced publication", r)
		}
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_checkpoint_publications`).Scan(&count); err != nil || count != 1 || f.resolverHit.Load() != provider {
		t.Fatal("publication duplicated or readprovider", count, err)
	}
}

func TestReviewCheckpointCLIInvalidBeforeCredentials(t *testing.T) {
	id := "review_publication_" + strings.Repeat("a", 32)
	evidence := "review_evidence_" + strings.Repeat("a", 32)
	cases := [][]string{{"reviews", "publication", "../bad"}, {"reviews", "publication", id, "--wait-seconds", "0"}, {"reviews", "publish", evidence, "--expect-revision", "-1", "--key", "k"}, {"reviews", "publish", evidence, "--expect-revision", "0", "--key", "k", "--retry-unposted"}, {"reviews", "publication-recover", id, "--expect-revision", "0", "--key", "k"}, {"reviews", "publication-recover", id, "--expect-revision", "9223372036854775807", "--key", "k"}}
	for _, suffix := range [][]string{{"--wait-seconds", "61"}, {"--wait-seconds", "01"}, {"--wait-seconds", "0", "--wait-seconds", "1"}, {"--retry-unposted=true"}, {"--retry-unposted", "--retry-unposted"}, {"--project", "other"}} {
		cases = append(cases, append([]string{"reviews", "publication-recover", id, "--expect-revision", "1", "--key", "k"}, suffix...))
	}
	for _, args := range cases {
		code, out, err := mutationInvoke(t, "/missing/checkpoint/profiles", "chosen", args...)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
			t.Fatal("invalid input reached credentials", args, code, out, err)
		}
	}
}

func TestReviewCheckpointCLIControlledTerminalWaitsStayFlat(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "checkpoint-wire")
	worker := f.worker(t, project, "Builder")
	id := "review_publication_" + strings.Repeat("a", 32)
	evidence := "review_evidence_" + strings.Repeat("a", 32)
	at := "2026-10-15T00:00:00Z"
	operation := client.ReviewCheckpointPublication{ID: id, ProjectID: project, EvidenceID: evidence, State: "held", Revision: 2, Reason: "capability_unavailable", SourceRevision: 1, CreatedAt: at, UpdatedAt: at, Anchor: client.ReviewAnchor{RepositoryID: 1, Repository: "owner/repo", PullNumber: 2, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), PatchSHA256: strings.Repeat("c", 64), BaseRef: "main", ObservedAt: at}}
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			f.handler.ServeHTTP(w, r)
			return
		}
		if r.Method == "POST" {
			posts++
			want := "wait=2"
			if posts == 2 {
				want = "wait=15"
				var request client.ReviewCheckpointRecovery
				if json.NewDecoder(r.Body).Decode(&request) != nil || !request.RetryUnposted || request.ExpectedRevision != 2 {
					t.Error("recovery request not preserved")
				}
			}
			if r.Header.Get("Prefer") != want {
				t.Error("explicit/default wait changed")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(operation)
	}))
	defer srv.Close()
	path := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	for _, args := range [][]string{{"reviews", "publish", evidence, "--expect-revision", "1", "--key", "default"}, {"reviews", "publication-recover", id, "--expect-revision", "2", "--key", "recovery", "--retry-unposted", "--wait-seconds", "15"}} {
		code, out, stderr := mutationInvoke(t, path, "builder", args...)
		if code != 0 || stderr != "" || strings.Count(out, "\n") != 1 || strings.Contains(out, `"Operation"`) || !strings.Contains(out, `"state":"held"`) {
			t.Fatal("terminal result wrapped/refused", code, out, stderr)
		}
	}
	if posts != 2 {
		t.Fatal("implicit requests", posts)
	}
}
