package reviewprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCIObservationReusesOnlySameObservationDiscovery(t *testing.T) {
	f := newCIObservationFixture(t, "merge")
	for pass := 1; pass <= 2; pass++ {
		got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
		if err != nil || got.State != "satisfied" {
			t.Fatal(got, err)
		}
		paths, counts := f.snapshot()
		for _, path := range paths {
			if strings.Contains(path, "check_suite_id=") {
				t.Fatalf("redundant suite discovery: %s", path)
			}
		}
		// Each observation still freshly reads the complete head inventory twice,
		// final direct current run once and exact-attempt jobs once. No cross-call reuse.
		if counts[actionsRunsPath] != 2*pass || counts[actionsRunsPath+"/50"] != pass || counts[actionsRunsPath+"/50/attempts/2/jobs"] != pass {
			t.Fatalf("fresh proof reads changed: %#v", counts)
		}
		t.Logf("pass=%d total HTTP requests=%d Actions discovery=%d", pass, len(paths), counts[actionsRunsPath])
	}
}

func TestDiscoveredActionsIdentityMustMatchFreshCurrentRun(t *testing.T) {
	for _, field := range []string{"id", "workflow_id", "run_number", "repository_id", "head", "suite"} {
		t.Run(field, func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.URL.Path != actionsCurrentPath {
					t.Error("identity mismatch fetched jobs or unrelated resource", r.URL.Path)
				}
				run := actionsRunFixture()
				switch field {
				case "repository_id":
					run["repository"].(map[string]any)["id"] = 21
				case "head":
					run["head_sha"] = strings.Repeat("b", 40)
				case "suite":
					run["check_suite_id"] = 41
				default:
					run[field] = 99
				}
				_ = json.NewEncoder(w).Encode(run)
			}))
			defer server.Close()
			discovered := ActionsHeadRun{ID: 50, WorkflowID: 30, RunNumber: 7, RepositoryID: 20, Repository: "owner/repo", HeadSHA: actionsHead, CheckSuiteID: 40}
			got, err := testGitHub(server, "synthetic-secret").currentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40, &discovered)
			if err == nil || got.State != "unproven" || len(got.Jobs) != 0 || reads.Load() != 1 {
				t.Fatal("discovered identity bypassed fresh proof", got, err, reads.Load())
			}
		})
	}
}

func TestCIObservationRejectsDuplicateSuiteBeforeDiscoveryReuse(t *testing.T) {
	f := newCIObservationFixture(t, "merge")
	var attemptReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == actionsRunsPath {
			head := r.URL.Query().Get("head_sha")
			first, duplicate := f.run(50, head), f.run(60, head)
			duplicate["check_suite_id"] = 40
			_ = json.NewEncoder(w).Encode(actionsEnvelope("workflow_runs", []map[string]any{first, duplicate}, 2))
			return
		}
		if strings.HasPrefix(r.URL.Path, actionsRunsPath+"/") {
			attemptReads.Add(1)
		}
		f.serve(w, r)
	}))
	defer server.Close()
	g := testGitHub(server, "synthetic-ci-observation-token")
	got, err := g.ObserveRequiredCI(t.Context(), ciObservationTarget())
	if err == nil || got.State != "unproven" || attemptReads.Load() != 0 {
		t.Fatal("ambiguous head inventory selected a run", got, err, attemptReads.Load())
	}
}
