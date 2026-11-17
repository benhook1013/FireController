package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

// The API deliberately supports a combined 384 KiB encoded patch. This is a
// real public-handler/SDK comparison, rather than a parser-only budget test.
func TestCombinedJobPatchAcrossClients(t *testing.T) {
	f := mutationFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	binding := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	path := profiles(t, map[string]profile.Context{"chosen": binding}, f.credential)
	session := reviewControlsStdio(t, path, "chosen")
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "fresh-combined-create", "title": "Combined text editing"}), &job)
	brief, summary, next := strings.Repeat("<", 32<<10), strings.Repeat("<", 8<<10), strings.Repeat("<", 8<<10)
	patch := map[string]any{"expected_revision": 1, "brief": brief, "progress": map[string]any{"summary": summary, "next": next}}
	encoded, _ := json.Marshal(patch)
	if len(encoded) <= 256<<10 || len(encoded) > 384<<10 {
		t.Fatal("combined encoded fixture outside documented API bound", len(encoded))
	}
	// The command accepts and sends the exact same logical content.
	var out, diagnostic bytes.Buffer
	code := cli.Run([]string{"--profiles-file", path, "--profile", "chosen", "jobs", "update", job.ID, "--expect-revision", "1", "--key", "fresh-combined-write", "--brief", brief, "--progress", summary, "--next", next}, nil, &out, &diagnostic)
	if code != cli.ExitSuccess || diagnostic.Len() != 0 {
		t.Fatalf("CLI did not support valid combined patch: code=%d diagnostic=%s", code, &diagnostic)
	}
	var receipt client.JobReceipt
	if json.Unmarshal(out.Bytes(), &receipt) != nil || receipt.Revision != 2 {
		t.Fatal("CLI did not commit one patch")
	}
	patch["job_id"], patch["idempotency_key"] = job.ID, "fresh-combined-write"
	// Same-key replay through MCP must recover the CLI receipt and must not
	// require splitting one atomic update into multiple domain revisions.
	var replay client.JobReceipt
	decodeResult(t, call(t, session, "job_update", patch), &replay)
	if replay != receipt {
		t.Fatal("cross-client exact replay changed receipt")
	}
	var current client.Job
	decodeResult(t, call(t, session, "job_read", map[string]any{"job_id": job.ID}), &current)
	if current.Revision != 2 || current.Brief != brief || current.Progress.Summary != summary || current.Progress.Next != next {
		t.Fatal("combined content did not survive exact recovery")
	}
	// The larger transport budget must not widen other mutation parsers or
	// admit a larger job body. All refusals leave this session usable.
	for _, invalid := range []struct {
		tool string
		body string
	}{
		{"jobs_create", `{"idempotency_key":"oversize-create","title":"Bounded","brief":"` + strings.Repeat("x", 256<<10) + `"}`},
		{"job_update", `{"job_id":"` + job.ID + `","idempotency_key":"oversize-patch","expected_revision":2,"brief":"` + strings.Repeat("x", client.JobPatchMaxEncodedBytes) + `"}`},
	} {
		errorResult(t, call(t, session, invalid.tool, json.RawMessage(invalid.body)), "invalid_arguments")
	}
	decodeResult(t, call(t, session, "job_read", map[string]any{"job_id": job.ID}), &current)
	if current.Revision != 2 {
		t.Fatal("oversized inputs changed durable state")
	}
	if _, err := client.DecodeJobPatch(append(bytes.Repeat([]byte(" "), client.JobPatchMaxEncodedBytes), []byte(`{"expected_revision":2,"status":"parked"}`)...)); err == nil {
		t.Fatal("job decoder admitted an above-contract encoded body")
	}
}
