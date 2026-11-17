package reviewprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInventoryInitialAttemptRetainsDirectFinalWitness(t *testing.T) {
	for _, mode := range []string{"stable", "attempt", "run", "workflow", "suite", "repo", "head", "event", "created", "status", "failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case actionsJobsPath:
					json.NewEncoder(w).Encode(actionsEnvelope("jobs", []map[string]any{actionsJobFixture(70)}, 1))
				case actionsCurrentPath:
					run := actionsRunFixture()
					switch mode {
					case "attempt":
						run["run_attempt"] = 3
					case "run":
						run["id"] = 51
					case "workflow":
						run["workflow_id"] = 31
					case "suite":
						run["check_suite_id"] = 41
					case "repo":
						run["repository"] = map[string]any{"id": 21, "full_name": "other/repo"}
					case "head":
						run["head_sha"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
					case "event":
						run["event"] = "push"
					case "created":
						run["created_at"] = "2025-12-31T23:59:00Z"
					case "status":
						run["status"] = "queued"
						run["conclusion"] = nil
					case "failure":
						run["conclusion"] = "failure"
					case "cancel":
						run["conclusion"] = "cancelled"
					}
					json.NewEncoder(w).Encode(run)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			created, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
			seed := ActionsHeadRun{ID: 50, WorkflowID: 30, RunNumber: 7, RunAttempt: 2, RepositoryID: 20, Repository: "owner/repo", HeadSHA: actionsHead, CheckSuiteID: 40, Event: "pull_request", Status: "completed", Conclusion: "success", CreatedAt: created, UpdatedAt: created.Add(time.Minute)}
			got, err := testGitHub(server, "synthetic-secret").collectActionsAttempt(t.Context(), "owner/repo", actionsHead, 40, &seed, true)
			if len(paths) != 2 || paths[0] != actionsJobsPath || paths[1] != actionsCurrentPath {
				t.Fatalf("missing exact jobs then direct final witness: %v", paths)
			}
			if mode == "stable" {
				if err != nil || got.State != "complete" || len(got.Jobs) != 1 {
					t.Fatal(got, err)
				}
			} else if got.State == "complete" || len(got.Jobs) != 0 {
				t.Fatal("stale inventory gained credit", got, err)
			}
		})
	}
}

func TestInventoryInitialAttemptIncompleteJobsAndCancellation(t *testing.T) {
	for _, mode := range []string{"incomplete_jobs", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == actionsJobsPath {
					if mode == "cancelled" {
						cancel()
						return
					}
					json.NewEncoder(w).Encode(actionsEnvelope("jobs", []map[string]any{actionsJobFixture(70)}, 2))
					return
				}
				t.Error("incomplete jobs reached direct final witness", r.URL.Path)
				json.NewEncoder(w).Encode(actionsRunFixture())
			}))
			defer server.Close()
			created, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
			seed := ActionsHeadRun{ID: 50, WorkflowID: 30, RunNumber: 7, RunAttempt: 2, RepositoryID: 20, Repository: "owner/repo", HeadSHA: actionsHead, CheckSuiteID: 40, Event: "pull_request", Status: "completed", Conclusion: "success", CreatedAt: created, UpdatedAt: created.Add(time.Minute)}
			got, err := testGitHub(server, "synthetic-secret").collectActionsAttempt(ctx, "owner/repo", actionsHead, 40, &seed, true)
			if err == nil || got.State == "complete" || len(got.Jobs) != 0 {
				t.Fatal("partial jobs gained credit", got, err)
			}
		})
	}
}
