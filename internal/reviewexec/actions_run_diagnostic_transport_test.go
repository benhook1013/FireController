package reviewexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/mcpadapter"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Real SDK and CLI requests traverse the typed client, authenticated HTTP,
// SQLite authorization and existing source workers. Only the upstream provider
// is controlled; this does not claim a packaged/live GitHub qualification.
func TestActionsRunDiagnosticActualSDKCLIWorkerRead(t *testing.T) {
	f, _, reader := runDiagnosticSetup(t)
	if _, err := f.store.DB().Exec(`INSERT INTO workers(id,project_id,name) VALUES('diagnostic-reader',?,'Reader')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	stop, done := refreshStart(t, f, reader)
	defer refreshStop(t, stop, done)
	mux := http.NewServeMux()
	f.e.Register(mux)
	mux.HandleFunc("GET /api/v1/instance", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
	})
	service := httptest.NewServer(mux)
	defer service.Close()
	dir := t.TempDir()
	credential := filepath.Join(dir, "credential")
	if err := os.WriteFile(credential, []byte(f.reader), 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "profiles": map[string]any{"chosen": map[string]string{"server_url": service.URL, "instance_id": f.store.InstanceID(), "project_id": attemptProject, "worker_id": "diagnostic-reader", "credential_file": credential}}})
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	beforeEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	beforeReceipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	adapter, err := mcpadapter.New([]string{"--profiles-file", path, "--profile", "chosen"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := mcp.NewInMemoryTransports()
	server, err := adapter.Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	sdk := mcp.NewClient(&mcp.Implementation{Name: "actions-diagnostic-test", Version: "1"}, nil)
	session, err := sdk.Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "actions_run_read", Arguments: map[string]any{"run_id": 50, "run_attempt": 2, "limit": 1}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var observed client.ActionsRunDiagnostic
	if err := json.Unmarshal(raw, &observed); err != nil || observed.ProjectID != attemptProject || observed.RepositoryID != 801 || observed.Total != 3 || observed.Omitted != 2 || len(observed.Jobs) != 1 || observed.Run.RunAttempt != 2 {
		t.Fatal(string(raw), err)
	}
	var out, diag bytes.Buffer
	code := cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "chosen", "reviews", "actions-run", "--run", "50", "--attempt", "2"}, nil, &out, &diag)
	var command client.ActionsRunDiagnostic
	if code != 0 || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &command) != nil || command.ProjectID != observed.ProjectID || command.Total != 3 || command.Pending != 1 || command.Running != 1 || command.Completed != 1 || command.Limit != 10 || command.Omitted != 0 {
		t.Fatal(code, out.String(), diag.String())
	}
	for _, raw := range []string{`{"limit":null}`, `{"limit":0}`, `{"age_hours":721}`, `{"repository":"foreign/repo"}`, `{"limit":1,"limit":2}`} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "actions_run_read", Arguments: json.RawMessage(raw)})
		if err != nil || !result.IsError {
			t.Fatal(raw, result, err)
		}
	}
	if reader.runCalls.Load() != 2 || reader.scans.Load() != 0 || reader.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events`) != beforeEvents || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != beforeReceipts || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) != 0 {
		t.Fatal("explicit read retried, expanded provider work or wrote native state", reader.runCalls.Load(), reader.calls.Load())
	}
}
