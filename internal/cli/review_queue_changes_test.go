package cli_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviews"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueueChangeCLIOrdinaryWriterAndLocalValidation(t *testing.T) {
	for _, args := range [][]string{{"queue-add"}, {"queue-add", "03", "--expect-config", "1", "--key", "k"}, {"queue-move", "2", "--expect-config", "1", "--key", "k"}, {"queue-add", "2", "--before", "2", "--expect-config", "1", "--key", "k"}, {"queue-add", "3", "--first", "--last", "--expect-config", "1", "--key", "k"}, {"queue-add", "3", "--before", "2", "--after", "4", "--expect-config", "1", "--key", "k"}, {"queue-add", "3", "--expect-config", "1", "--paused", "false", "--key", "k"}} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing/profiles", "reviews"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(args, code, stderr)
		}
	}
	for _, name := range []string{"queue-add", "queue-move", "queue-remove"} {
		code, out, stderr := run([]string{"reviews", name, "--help"}, nil)
		if code != 0 || stderr != "" || !strings.Contains(out, "Project write access") {
			t.Fatal(code, out, stderr)
		}
		if name == "queue-remove" && (!strings.Contains(out, "--reason superseded") || !strings.Contains(out, "Defunct merged PRs remain refused")) {
			t.Fatal("local help omitted deliberate merged-carrier boundary", out)
		}
	}
	f, project, worker := reviewExecutionFixture(t, true)
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	reviews.Register(mux, f.store)
	f.handler = mux
	server := httptest.NewServer(f.handler)
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	invoke := func(args ...string) client.ReviewRequestReceipt {
		t.Helper()
		code, out, stderr := mutationInvoke(t, profiles, "builder", append([]string{"reviews"}, args...)...)
		var receipt client.ReviewRequestReceipt
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &receipt) != nil {
			t.Fatal(code, out, stderr)
		}
		return receipt
	}
	first := invoke("queue-add", "3", "--before", "2", "--expect-config", "1", "--key", "add-3")
	if first.ID != project || first.Revision != 2 || first.State != "configured" {
		t.Fatal(first)
	}
	moved := invoke("queue-move", "3", "--last", "--expect-config", "2", "--key", "move-3")
	if moved.Revision != 3 {
		t.Fatal(moved)
	}
	if replay := invoke("queue-add", "3", "--before", "2", "--expect-config", "1", "--key", "add-3"); replay != first {
		t.Fatal(replay)
	}
	noop := invoke("queue-move", "3", "--last", "--expect-config", "3", "--key", "move-noop")
	if noop.State != "unchanged" || noop.Revision != 3 {
		t.Fatal(noop)
	}
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, project).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	f.call(t, "POST", "/api/v1/projects/"+project+"/reviews/observations", f.admin, "", reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 1, Source: "synthetic merged carrier", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{{Number: 2, Title: "Original PR", State: "open", CIState: "unknown", Mergeability: "unknown"}, {Number: 3, Title: "Absorbed carrier", State: "merged", CIState: "unknown", Mergeability: "unknown"}}})
	code, historicalJSON, stderr := mutationInvoke(t, profiles, "builder", "reviews", "pull", "3")
	var historical client.ReviewPull
	if code != 0 || stderr != "" || json.Unmarshal([]byte(historicalJSON), &historical) != nil || historical.Pull.State != "merged" {
		t.Fatal("fixture carrier was not merged", code, historicalJSON, stderr)
	}
	code, out, stderr := mutationInvoke(t, profiles, "builder", "reviews", "queue-remove", "3", "--reason", "defunct", "--expect-config", "3", "--key", "merged-defunct")
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "review_queue_merged") || !strings.Contains(stderr, "reason=superseded") || !strings.Contains(stderr, "preserves all history") {
		t.Fatal("merged refusal lost its supported deliberate recovery", code, out, stderr)
	}
	removed := invoke("queue-remove", "3", "--reason", "superseded", "--expect-config", "3", "--key", "remove-3")
	if removed.Revision != 4 {
		t.Fatal(removed)
	}
	if replay := invoke("queue-remove", "3", "--reason", "superseded", "--expect-config", "3", "--key", "remove-3"); replay != removed {
		t.Fatal(replay)
	}
	code, retainedJSON, stderr := mutationInvoke(t, profiles, "builder", "reviews", "pull", "3", "--snapshot", historical.SnapshotID)
	if code != 0 || stderr != "" || retainedJSON != historicalJSON {
		t.Fatal("superseded carrier lost retained history", code, retainedJSON, stderr)
	}
	final := invoke("queue-remove", "2", "--reason", "defunct", "--expect-config", "4", "--key", "remove-final")
	if final.Revision != 5 {
		t.Fatal(final)
	}
	code, out, stderr = mutationInvoke(t, profiles, "builder", "reviews", "execution")
	var empty client.ReviewExecutionConfiguration
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &empty) != nil || empty.Queue == nil || len(empty.Queue) != 0 {
		t.Fatal(code, out, stderr)
	}

}

func TestQueueRemoveCLIRejectsInvalidInputBeforeCredentials(t *testing.T) {
	for _, args := range [][]string{
		{"2", "--expect-config", "1", "--key", "k"},
		{"2", "--reason", "merged", "--expect-config", "1", "--key", "k"},
		{"2", "--reason", "defunct", "--first", "--expect-config", "1", "--key", "k"},
		{"2", "--reason", "defunct", "--after", "3", "--expect-config", "1", "--key", "k"},
	} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing/profiles", "reviews", "queue-remove"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(args, code, out, stderr)
		}
	}
}
