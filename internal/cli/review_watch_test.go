package cli_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewWatchCLIActualSnapshotReplayAndExecutionComposition(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	providers := f.resolverHit.Load()
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "watch", "--channel", "cli", "--limit", "1")
	var snapshot client.ReviewWatchResult
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &snapshot) != nil || snapshot.Snapshot == nil || snapshot.EventCursor == "" {
		t.Fatal(code, out, stderr)
	}
	queued := mutationSuccess(t, path, "builder", reviewRequestArgs()...)
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "watch", "--channel", "cli", "--after", snapshot.EventCursor, "--wait-seconds", "0")
	var changed client.ReviewWatchResult
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &changed) != nil || len(changed.Changes) != 1 || changed.Changes[0].Operation.ID != queued["id"] {
		t.Fatal(code, out, stderr)
	}
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "execution")
	var configuration client.ReviewExecutionOutput
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &configuration) != nil || configuration.Scheduling.ProjectID != project || configuration.Scheduling.Revision != 0 || configuration.Revision != 1 {
		t.Fatal(code, out, stderr)
	}
	if f.resolverHit.Load() != providers {
		t.Fatal("read caused provider work")
	}
	for _, args := range [][]string{{"--after", ""}, {"--limit", "0"}, {"--wait-seconds", "901"}, {"--pr", "0"}, {"--channel", "other"}, {"--cursor", "pagination"}, {"--after", "one", "--after", "two"}} {
		code, _, stderr = mutationInvoke(t, path, "builder", append([]string{"reviews", "watch"}, args...)...)
		if code == 0 || !strings.Contains(stderr, "invalid") {
			t.Fatal("invalid arguments accepted", args, code, stderr)
		}
	}
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "watch", "--help")
	if code != 0 || stderr != "" || !strings.Contains(out, "event_cursor") || !strings.Contains(out, "900") {
		t.Fatal("watch help unavailable", code, out, stderr)
	}
}
