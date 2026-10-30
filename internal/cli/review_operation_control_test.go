package cli_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewOperationControlCLIActualRevisionReplayAndBinding(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t, true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/control") && (r.Header.Get("X-FC-Worker") != worker || !strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+project+"/")) {
			t.Error("control lost identity/project")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	queued := mutationSuccess(t, profiles, "builder", reviewRequestArgs()...)
	id := queued["id"].(string)
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
	code, out, stderr := mutationInvoke(t, profiles, "builder", "reviews", "operation-control", id, "--action", "resume", "--expect-revision", "1", "--key", "stale")
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
		if code != 0 || stderr != "" || !strings.Contains(out, "undispatched") || !strings.Contains(out, "--expect-revision") {
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
