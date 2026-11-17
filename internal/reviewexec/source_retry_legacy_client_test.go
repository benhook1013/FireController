package reviewexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSourceRetryActualPriorClientReadsCurrentServer(t *testing.T) {
	executable := os.Getenv("FC_TEST_SOURCE_LEGACY_CLIENT")
	if executable == "" {
		t.Skip("selected older package is an operator integration proof")
	}
	f, r, now := sourceFixture(t)
	a := scheduleSourceFixture(t, f, r, *now)
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'version-reader','Reader')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get(operationWaitHeader) != "2" {
			t.Error("selected older binary did not advertise prior wait contract")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	directory := t.TempDir()
	token := filepath.Join(directory, "fixture.token")
	profile := filepath.Join(directory, "profiles.json")
	if err := os.WriteFile(token, []byte(f.writer), 0600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"version": 1, "profiles": map[string]any{"fixture": map[string]any{"server_url": server.URL, "instance_id": f.store.InstanceID(), "project_id": attemptProject, "worker_id": "version-reader", "credential_file": token}}})
	if err := os.WriteFile(profile, body, 0600); err != nil {
		t.Fatal(err)
	}
	for _, diagnostics := range []bool{false, true} {
		args := []string{"--profiles-file", profile, "--profile", "fixture", "reviews", "operation", a.OperationID}
		if diagnostics {
			args = append(args, "--diagnostics")
		}
		command := exec.CommandContext(t.Context(), executable, args...)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			t.Fatal("actual prior client rejected current queued wait", err, stderr.String())
		}
		var out OperationStatus
		if json.Unmarshal(stdout.Bytes(), &out) != nil || out.ID != a.OperationID || out.State != "queued" || out.AttemptID != "" || out.Wait == nil || out.Wait.BaselineAttemptID != "" {
			t.Fatal("actual prior client lost truthful queued state", stdout.String())
		}
	}
}
