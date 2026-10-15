package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviews"
)

func reviewObservation() reviews.ObservationRequest {
	zero := int64(0)
	observation := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "synthetic CLI fixture", ObservedAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)}
	for i := 1; i <= 6; i++ {
		pull := reviews.PullInput{Number: int64(i), Title: "Stored PR " + strconv.Itoa(i), State: "open", HeadSHA: strings.Repeat("a", 40), CIState: "unknown", Mergeability: "unknown", Complete: false}
		if i == 1 {
			pull.State = "merged"
			for j := 0; j < 6; j++ {
				run := reviews.RunInput{SourceRunID: "run-" + strconv.Itoa(j), Channel: "manual", HeadSHA: strings.Repeat("b", 40), Reviewer: "synthetic reviewer", Scope: "selected scope", Outcome: "unknown", Found: 1, CoverageLimits: []string{"Incomplete historical evidence; no readiness claim."}}
				if j == 5 {
					run.Findings = []reviews.Finding{{SourceFindingID: "finding-one", Title: "Explicit detail", Detail: "PRIVATE_FINDING_DETAIL", Severity: "medium", Disposition: "accepted", DecisionReason: "Reproduced", FixSHA: strings.Repeat("c", 40), ProofNote: "Focused test"}}
					run.Accepted = 1
				}
				pull.Runs = append(pull.Runs, run)
			}
			pull.Attempts = []reviews.Attempt{{SourceAttemptID: "unknown-duration", Channel: "manual", State: "unknown", SourceRunID: "run-5"}}
		}
		observation.Queue = append(observation.Queue, pull)
	}
	return observation
}

func TestReviewCommandsReadNativeSnapshotWithoutEffects(t *testing.T) {
	f := setupServiceFixture(t)
	if err := reviews.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	reviews.Register(mux, f.store)
	f.handler = mux
	project := f.project(t, "cli-reviews")
	worker := f.worker(t, project, "Builder")
	var reads, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		if strings.Contains(r.URL.Path, "/reviews/") {
			reads.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	invoke := func(dst any, args ...string) string {
		t.Helper()
		code, out, stderr := mutationInvoke(t, path, "builder", append([]string{"reviews"}, args...)...)
		if code != cli.ExitSuccess || stderr != "" || json.Unmarshal([]byte(out), dst) != nil {
			t.Fatalf("review command: %d %s %s", code, out, stderr)
		}
		return out
	}
	var empty client.ReviewQueue
	invoke(&empty, "queue")
	if empty.Configured || len(empty.Items) != 0 || empty.Revision != 0 {
		t.Fatal("unconfigured review workspace was hidden")
	}
	f.call(t, "POST", "/api/v1/projects/"+project+"/reviews/observations", f.admin, "", reviewObservation())
	counts := func() [2]int {
		t.Helper()
		var out [2]int
		for i, table := range []string{"events", "receipts"} {
			if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&out[i]); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before, providerBefore := counts(), f.resolverHit.Load()
	var queue, tail client.ReviewQueue
	out := invoke(&queue, "queue")
	if len(queue.Items) != 5 || !queue.HasMore || queue.Cursor == "" || queue.Items[0].State != "merged" || strings.Contains(out, "PRIVATE_FINDING_DETAIL") {
		t.Fatal("default five-item compact queue or retained merged history changed")
	}
	invoke(&tail, "queue", "--cursor", queue.Cursor)
	if len(tail.Items) != 1 || tail.HasMore || tail.SnapshotID != queue.SnapshotID || tail.Items[0].Number != 6 {
		t.Fatal("queue continuation was lost")
	}
	var pull client.ReviewPull
	invoke(&pull, "pull", "1", "--snapshot", queue.SnapshotID)
	if len(pull.Attempts) != 1 || pull.Attempts[0].DurationSeconds != nil || pull.Pull.Complete {
		t.Fatal("unknown duration or incomplete source observation was repaired")
	}
	var runs, last client.ReviewRuns
	out = invoke(&runs, "runs", "1", "--snapshot", queue.SnapshotID)
	if len(runs.Items) != 5 || !runs.HasMore || runs.Items[0].SourceRunID != "run-5" || strings.Contains(out, "PRIVATE_FINDING_DETAIL") {
		t.Fatal("run summaries were not compact/newest-first/default-five")
	}
	invoke(&last, "runs", "1", "--snapshot", queue.SnapshotID, "--cursor", runs.Cursor)
	if len(last.Items) != 1 || last.Items[0].SourceRunID != "run-0" || last.HasMore {
		t.Fatal("run pagination lost an item")
	}
	var detail client.ReviewRunDetail
	invoke(&detail, "run", runs.Items[0].ID, "--snapshot", queue.SnapshotID)
	if len(detail.Findings) != 1 || detail.Findings[0].Detail != "PRIVATE_FINDING_DETAIL" || detail.Run.Complete || len(detail.Run.CoverageLimits) != 1 || detail.SnapshotID != queue.SnapshotID {
		t.Fatal("explicit finding/proof/incomplete source detail changed")
	}
	if writes.Load() != 0 || reads.Load() != 7 || f.resolverHit.Load() != providerBefore || !reflect.DeepEqual(before, counts()) {
		t.Fatal("read adapter performed extra requests, provider work or durable effects")
	}
}

func TestReviewCommandSelectorsFailBeforeProfileOrNetwork(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	path := fixture(t, encode(t, map[string]any{"version": 1, "profiles": map[string]any{"selected": map[string]any{
		"server_url": service.URL, "instance_id": "instance-one", "project_id": "project-one", "worker_id": "worker-one", "credential_file": "absent-private-credential",
	}}}))
	for i, args := range [][]string{
		{"queue", "--snapshot", "x"}, {"queue", "extra"}, {"queue", "--limit", "0"}, {"queue", "--limit", "101"}, {"queue", "--limit", "1", "--limit", "2"},
		{"queue", "--cursor", ""}, {"queue", "--cursor", " padded "}, {"queue", "--cursor", "bad\x00"}, {"queue", "--cursor", strings.Repeat("x", 4097)},
		{"pull"}, {"pull", "0"}, {"pull", "01"}, {"pull", "+1"}, {"pull", "2147483648"}, {"pull", "1", "--limit", "1"},
		{"runs", "1", "--snapshot", ""}, {"runs", "1", "--snapshot", "review_snapshot_" + strings.Repeat("A", 32)},
		{"run", "../other"}, {"run", "review_run_" + strings.Repeat("a", 32), "--snapshot", "x", "--snapshot", "y"},
		{"observe"}, {"queue", "--project", "other"},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			for _, profilesPath := range []string{"/unavailable/profiles.json", path} {
				invocation := append([]string{"--profile", "selected", "--profiles-file", profilesPath, "reviews"}, args...)
				code, out, stderr := run(invocation, nil)
				if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") || strings.Contains(stderr, "absent-private-credential") {
					t.Fatalf("selector validation precedence: %d %s %s", code, out, stderr)
				}
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("invalid review selectors reached HTTP")
	}
}
