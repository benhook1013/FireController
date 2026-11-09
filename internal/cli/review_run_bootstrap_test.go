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
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewRunCLIIdentityFloorForDefaultAndShortWaits(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "selection-bootstrap")
	worker := f.worker(t, project, "Builder")
	for _, command := range []string{"start", "run-read"} {
		for _, wait := range []string{"default", "0", "2", "cancel-before", "cancel-after", "earlier-deadline"} {
			t.Run(command+"/"+wait, func(t *testing.T) {
				t.Parallel()
				var identities, operations atomic.Int64
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				selected := "0"
				if command == "start" {
					selected = "2"
				}
				if wait == "0" || wait == "2" {
					selected = wait
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						if identities.Add(1) < 7 {
							http.Error(w, "synthetic gateway restart", http.StatusServiceUnavailable)
							return
						} // Seven attempts consume 7.75 seconds of bounded identity backoff.
						f.handler.ServeHTTP(w, r)
						return
					}
					operations.Add(1)
					if r.Header.Get("Prefer") != "wait="+selected || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("Authorization") != "Bearer "+f.admin {
						t.Error("wait or binding changed")
					}
					if command == "start" {
						body, err := io.ReadAll(r.Body)
						if err != nil || r.Method != "POST" || r.URL.Path != "/api/v1/projects/"+project+"/reviews/selections" || r.Header.Get("Idempotency-Key") != "original-selection-key" || string(body) != `{"channel":"hosted"}` {
							t.Error("mutation changed original key/body", string(body), err)
						}
					} else if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/reviews/selections/"+cliSelectionID) || r.Header.Get("Idempotency-Key") != "" {
						t.Error("selection read became mutation")
					}
					if wait == "cancel-after" {
						cancel()
						<-r.Context().Done()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(client.ReviewRunState{Selection: client.ReviewRunSelection{ID: cliSelectionID, ProjectID: project, ActorID: "principal_one", WorkerID: worker, Channel: "hosted", ConfigurationRevision: 1, PolicyRevision: 1, State: "empty", Reason: "queue_complete", Revision: 2, CreatedAt: "2026-10-18T01:00:00Z", UpdatedAt: "2026-10-18T01:01:00Z"}})
				}))
				defer srv.Close()
				profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
				args := []string{"--profiles-file", profiles, "reviews", command}
				if command == "start" {
					args = append(args, "--channel", "hosted", "--key", "original-selection-key")
				} else {
					args = append(args, "--selection-id", cliSelectionID)
				}
				if wait == "0" || wait == "2" {
					args = append(args, "--wait-seconds", wait)
				}
				if wait == "cancel-before" {
					cancel()
				}
				if wait == "earlier-deadline" {
					var deadlineCancel context.CancelFunc
					ctx, deadlineCancel = context.WithTimeout(ctx, 50*time.Millisecond)
					defer deadlineCancel()
				}
				var out, diagnostic strings.Builder
				code := cli.RunContext(ctx, args, map[string]string{"FC_PROFILE": "builder"}, &out, &diagnostic)
				if wait == "earlier-deadline" {
					if code == cli.ExitSuccess || operations.Load() != 0 || !strings.Contains(diagnostic.String(), "deadline_exceeded") || strings.Contains(diagnostic.String(), "outcome_unknown") {
						t.Fatal("earlier caller deadline lost precedence", code, operations.Load(), diagnostic.String())
					}
					return
				}
				if wait == "cancel-before" {
					if code != cli.ExitCancelled || identities.Load() != 0 || operations.Load() != 0 || strings.Contains(diagnostic.String(), "outcome_unknown") {
						t.Fatal("pre-dispatch cancellation sent work", code, identities.Load(), operations.Load(), diagnostic.String())
					}
					return
				}
				if identities.Load() != 7 || operations.Load() != 1 {
					t.Fatal("slow bootstrap failed or selected operation replayed", code, identities.Load(), operations.Load(), diagnostic.String())
				}
				if wait == "cancel-after" {
					unknown := strings.Contains(diagnostic.String(), `"outcome_unknown":true`)
					if code != cli.ExitCancelled || out.Len() != 0 || unknown != (command == "start") {
						t.Fatal("post-dispatch cancellation changed uncertainty", code, out.String(), diagnostic.String())
					}
					return
				}
				if code != cli.ExitSuccess || diagnostic.Len() != 0 || !strings.Contains(out.String(), cliSelectionID) {
					t.Fatal("short wait lost bootstrap floor", code, out.String(), diagnostic.String())
				}
			})
		}
	}
}
