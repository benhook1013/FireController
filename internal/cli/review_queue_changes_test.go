package cli_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueueChangeCLIOrdinaryWriterAndLocalValidation(t *testing.T) {
	for _, args := range [][]string{{"queue-add"}, {"queue-add", "03", "--expect-config", "1", "--key", "k"}, {"queue-move", "2", "--expect-config", "1", "--key", "k"}, {"queue-add", "2", "--before", "2", "--expect-config", "1", "--key", "k"}, {"queue-add", "3", "--first", "--last", "--expect-config", "1", "--key", "k"}, {"queue-add", "3", "--before", "2", "--after", "4", "--expect-config", "1", "--key", "k"}, {"queue-add", "3", "--expect-config", "1", "--paused", "false", "--key", "k"}} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing/profiles", "reviews"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(args, code, stderr)
		}
	}
	for _, name := range []string{"queue-add", "queue-move"} {
		code, out, stderr := run([]string{"reviews", name, "--help"}, nil)
		if code != 0 || stderr != "" || !strings.Contains(out, "Project write access") {
			t.Fatal(code, out, stderr)
		}
	}
	f, project, worker := reviewExecutionFixture(t)
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
}
