package cli_test

import (
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReviewOperationControlCLIActualRevisionReplayAndBinding(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/control") && (r.Header.Get("X-FC-Worker") != worker || !strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+project+"/")) {
			t.Error("control lost identity/project")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	code, out, stderr := mutationInvoke(t, profiles, "builder", reviewRequestArgs()...)
	var queued struct {
		ID         string                            `json:"id"`
		State      string                            `json:"state"`
		Revision   int64                             `json:"revision"`
		Diagnostic *client.ReviewOperationDiagnostic `json:"diagnostic,omitempty"`
	}
	if code != 0 || stderr != "" || strictjson.Decode([]byte(out), &queued) != nil || queued.State != "queued" || queued.Revision != 1 || !strings.HasPrefix(queued.ID, "review_operation_") {
		t.Fatal("invalid adapted queued receipt", code, out, stderr)
	}
	id := queued.ID
	invoke := func(action, rev, key string) client.ReviewOperationControlReceipt {
		t.Helper()
		code, out, stderr := mutationInvoke(t, profiles, "builder", "reviews", "operation-control", id, "--action", action, "--expect-revision", rev, "--key", key)
		var v client.ReviewOperationControlReceipt
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &v) != nil {
			t.Fatal(code, out, stderr)
		}
		return v
	}
	paused := invoke("pause", "1", "pause")
	if !paused.Paused || paused.Revision != 2 {
		t.Fatal(paused)
	}
	if replay := invoke("pause", "1", "pause"); replay != paused {
		t.Fatal("replay changed", replay)
	}
	code, out, stderr = mutationInvoke(t, profiles, "builder", "reviews", "operation-control", id, "--action", "resume", "--expect-revision", "1", "--key", "stale")
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "review_conflict") {
		t.Fatal(code, out, stderr)
	}
	code, out, stderr = mutationInvoke(t, profiles, "builder", "reviews", "operation", id)
	if code != 0 || !strings.Contains(out, `"wait":{"paused":true}`) {
		t.Fatal(code, out, stderr)
	}
	resumed := invoke("resume", "2", "resume")
	if resumed.Paused || resumed.Revision != 3 {
		t.Fatal(resumed)
	}
	cancelled := invoke("cancel", "3", "cancel")
	if cancelled.State != "cancelled" || cancelled.Revision != 4 {
		t.Fatal(cancelled)
	}
}

func TestReviewOperationControlCLIHelpAndMalformedOffline(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	for _, args := range [][]string{{"reviews", "operation-control", "--help"}, {"help", "reviews", "operation-control"}} {
		code, out, stderr := mutationInvoke(t, "/missing/private-profiles", "absent", args...)
		if code != 0 || stderr != "" || !strings.Contains(out, "undispatched") || !strings.Contains(out, "--expect-revision") || !strings.Contains(out, "docs/retained-review-quota-controls.md") {
			t.Fatal(code, out, stderr)
		}
	}
	for _, extra := range [][]string{{id, "--action", "pause", "--expect-revision", "1"}, {id, "--action", "stop", "--expect-revision", "1", "--key", "x"}, {id, "--action", "pause", "--expect-revision", "01", "--key", "x"}, {id, "--action", "pause", "--expect-revision", "1", "--key", "x", "--worker", "other"}, {id, "--action", "pause", "--action", "resume", "--expect-revision", "1", "--key", "x"}} {
		args := append([]string{"reviews", "operation-control"}, extra...)
		code, out, stderr := mutationInvoke(t, "/missing/private-profiles", "absent", args...)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(code, out, stderr)
		}
	}
}

func TestReviewControlOutputFailureRetainsSameKeyRecovery(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	code, out, stderr := mutationInvoke(t, profiles, "builder", reviewRequestArgs()...)
	var queued struct {
		ID string `json:"id"`
	}
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &queued) != nil {
		t.Fatal(code, out, stderr)
	}
	args := []string{"--profiles-file", profiles, "--profile", "builder", "reviews", "operation-control", queued.ID, "--action", "pause", "--expect-revision", "1", "--key", "original-control-key"}
	var diagnostic strings.Builder
	code = cli.RunContext(context.Background(), args, map[string]string{}, roundFailWriter{}, &diagnostic)
	if code != cli.ExitConfig || !strings.Contains(diagnostic.String(), `"outcome_unknown":true`) || !strings.Contains(diagnostic.String(), "exact same key") {
		t.Fatal(code, diagnostic.String())
	}
	code, out, stderr = mutationInvoke(t, profiles, "builder", args[4:]...)
	var receipt client.ReviewOperationControlReceipt
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &receipt) != nil || !receipt.Paused || receipt.Revision != 2 {
		t.Fatal("same-key recovery changed effect", code, out, stderr)
	}
}

func TestReviewSchedulingCLIOutputFailureRetainsSameKeyRecovery(t *testing.T) {
	for _, action := range []string{"configure", "pause", "resume"} {
		for _, output := range []struct {
			name   string
			writer io.Writer
		}{{"failed", roundFailWriter{}}, {"short", shortMapOutput{}}} {
			t.Run(action+"/"+output.name, func(t *testing.T) {
				f, project, worker := reviewExecutionFixture(t)
				var posts atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scheduling") {
						posts.Add(1)
						if r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || !strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+project+"/") {
							t.Error("scheduling mutation lost binding")
						}
					}
					f.handler.ServeHTTP(w, r)
				}))
				defer server.Close()
				profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
				expected, revision := "0", int64(1)
				if action == "resume" {
					mutationSuccess(t, profiles, "builder", "reviews", "scheduling", "pause", "--pr", "2", "--expect-revision", "0", "--key", "prepare-paused-pull")
					expected, revision = "1", 2
					posts.Store(0)
				}
				args := []string{"reviews", "scheduling", action, "--expect-revision", expected, "--key", "original-scheduling-key"}
				state, id := "pull_scheduling_updated", "2"
				switch action {
				case "configure":
					args = append(args, "--automatic-hosted", "true")
					state, id = "scheduling_configured", project
				case "pause", "resume":
					args = append(args, "--pr", "2")
				}
				full := append([]string{"--profiles-file", profiles, "--profile", "builder"}, args...)
				var diag strings.Builder
				code := cli.RunContext(t.Context(), full, nil, output.writer, &diag)
				var failure struct {
					Error client.Error `json:"error"`
				}
				if code != cli.ExitConfig || json.Unmarshal([]byte(diag.String()), &failure) != nil || failure.Error.Code != "output_failed" || !failure.Error.OutcomeUnknown || !strings.Contains(failure.Error.Action, "exact same key, request and bound instance") || posts.Load() != 1 {
					t.Fatal("committed scheduling output failure lost exact recovery or retried", code, diag.String(), posts.Load())
				}
				code, out, stderr := mutationInvoke(t, profiles, "builder", args...)
				var receipt client.ReviewRequestReceipt
				if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &receipt) != nil || receipt.ID != id || receipt.State != state || receipt.Revision != revision || posts.Load() != 2 {
					t.Fatal("exact recovery changed the scheduling effect", code, out, stderr, posts.Load())
				}
				code, out, stderr = mutationInvoke(t, profiles, "builder", args...)
				var replay client.ReviewRequestReceipt
				if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &replay) != nil || !reflect.DeepEqual(receipt, replay) || posts.Load() != 3 {
					t.Fatal("scheduling replay changed receipt", code, out, stderr)
				}
			})
		}
	}
}

func TestReviewSchedulingCLIReadOutputFailureDoesNotClaimMutation(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	for _, suffix := range [][]string{nil, {"read"}, {"read", "--pr", "2"}} {
		args := append([]string{"--profiles-file", profiles, "--profile", "builder", "reviews", "scheduling"}, suffix...)
		var diag strings.Builder
		code := cli.RunContext(t.Context(), args, nil, roundFailWriter{}, &diag)
		if code != cli.ExitConfig || !strings.Contains(diag.String(), "output_failed") || strings.Contains(diag.String(), "outcome_unknown") || strings.Contains(diag.String(), "same key") {
			t.Fatal("read output failure gained mutation recovery semantics", code, diag.String())
		}
	}
}
