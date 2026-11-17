package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestClientProvenanceMatchesAcrossLocalAndVerifiedDiagnostics(t *testing.T) {
	f := mutationFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	binding.InstanceID = f.store.InstanceID()
	binding.ProjectID = f.project
	binding.WorkerID = f.sender
	path := profiles(t, map[string]profile.Context{"chosen": binding}, f.credential)
	run := func(args ...string) map[string]any {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := cli.Run(args, nil, &out, &stderr); code != cli.ExitSuccess || stderr.Len() != 0 {
			t.Fatal("diagnostic failed", code, stderr.String())
		}
		var v map[string]any
		if json.Unmarshal(out.Bytes(), &v) != nil {
			t.Fatal("invalid diagnostic JSON")
		}
		return v
	}
	version := run("version")
	session := connect(t, adapter(t, path, "chosen"), "")
	for _, name := range []string{"context", "doctor"} {
		cliValue := run("--profiles-file", path, "--profile", "chosen", name)
		var mcpValue map[string]any
		decodeResult(t, call(t, session, name, map[string]any{}), &mcpValue)
		for _, field := range []string{"version", "source_revision", "source_state"} {
			want, ok := version[field].(string)
			if !ok || want == "" || cliValue["client_"+field] != want || mcpValue["client_"+field] != want {
				t.Fatal("running client provenance missing or inconsistent", name, field)
			}
		}
		if cliValue["worker_id"] != f.sender || mcpValue["worker_id"] != f.sender {
			t.Fatal("diagnostics changed binding")
		}
		if name == "doctor" && (cliValue["verified"] != true || mcpValue["verified"] != true) {
			t.Fatal("doctor lost verification")
		}
	}
	if f.providers.Load() != 1 {
		t.Fatal("diagnostics made provider requests")
	}
}
