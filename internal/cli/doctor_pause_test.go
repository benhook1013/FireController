package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestDoctorReturnsCurrentPauseWithOneDomainRead(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "doctor-pause")
	worker := f.worker(t, project, "Builder")
	const readToken = "synthetic-doctor-read-token"
	f.grant(t, project, readToken)
	var instances, pauses, unexpected atomic.Int64
	var fault atomic.Int32
	pausePath := "/api/v1/projects/" + project + "/workers/" + worker + "/pause"
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			unexpected.Add(1)
		}
		switch r.URL.Path {
		case "/api/v1/instance":
			instances.Add(1)
			if fault.Load() == 1 {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"instance_id": "different-instance"})
				return
			}
		case pausePath, "/api/v1/projects/" + project + "/workers/worker-missing/pause":
			pauses.Add(1)
			if fault.Load() == 2 {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(client.WorkerPause{WorkerID: "wrong-worker"})
				return
			}
		default:
			unexpected.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	credential := writeCredential(t, t.TempDir(), "read-only", readToken)
	entries := map[string]any{}
	for name, id := range map[string]string{"builder": worker, "missing": "worker-missing"} {
		entries[name] = map[string]string{"server_url": service.URL, "instance_id": f.store.InstanceID(), "project_id": project, "worker_id": id, "credential_file": credential}
	}
	profilesPath := fixture(t, encode(t, map[string]any{"version": 1, "profiles": entries}))
	snapshot := func() string {
		t.Helper()
		state := f.call(t, "GET", pausePath, f.admin, "", nil)
		counts := make([]int, 3)
		for i, table := range []string{"worker_pause_history", "events", "receipts"} {
			if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&counts[i]); err != nil {
				t.Fatal(err)
			}
		}
		raw, _ := json.Marshal(state)
		return string(raw) + fmt.Sprint(counts)
	}
	prefix := " \tPRIVATE_WAIT π\n"
	reason := prefix + strings.Repeat("<", 1024-len(prefix))
	for i, want := range []client.WorkerPause{{WorkerID: worker}, {WorkerID: worker, Revision: 1, Paused: true, Reason: reason}, {WorkerID: worker, Revision: 2, Paused: true}, {WorkerID: worker, Revision: 3}} {
		if i > 0 {
			f.call(t, "PUT", pausePath, f.admin, worker, map[string]any{"expected_revision": i - 1, "paused": want.Paused, "reason": want.Reason})
		}
		before := snapshot()
		instances.Store(0)
		pauses.Store(0)
		code, out, stderr := mutationInvoke(t, profilesPath, "builder", "doctor")
		var raw map[string]json.RawMessage
		var got struct {
			Verified    bool               `json:"verified"`
			WorkerID    string             `json:"worker_id"`
			WorkerPause client.WorkerPause `json:"worker_pause"`
		}
		if code != cli.ExitSuccess || stderr != "" || len(out) > 10<<10 || json.Unmarshal([]byte(out), &raw) != nil || json.Unmarshal([]byte(out), &got) != nil || !got.Verified || got.WorkerID != worker || got.WorkerPause != want {
			t.Fatalf("doctor state %d: %d %s %s", i, code, out, stderr)
		}
		var pauseFields map[string]json.RawMessage
		if len(raw) != 10 || json.Unmarshal(raw["worker_pause"], &pauseFields) != nil || len(pauseFields) != 4 {
			t.Fatalf("required fields absent: %s", out)
		}
		for _, field := range []string{"profile", "server_url", "instance_id", "project_id", "worker_id", "verified", "client_version", "client_source_revision", "client_source_state"} {
			if len(raw[field]) == 0 {
				t.Fatalf("original doctor field %s missing", field)
			}
		}
		if instances.Load() != 1 || pauses.Load() != 1 || unexpected.Load() != 0 || snapshot() != before {
			t.Fatal("doctor added reads or changed durable pause/history/receipts/events")
		}
	}
	for _, spec := range []struct {
		fault       int32
		code        string
		domainReads int64
	}{{1, "instance_mismatch", 0}, {2, "invalid_response", 1}} {
		fault.Store(spec.fault)
		instances.Store(0)
		pauses.Store(0)
		before := snapshot()
		code, out, stderr := mutationInvoke(t, profilesPath, "builder", "doctor")
		if code == cli.ExitSuccess || out != "" || !strings.Contains(stderr, `"code":"`+spec.code+`"`) || strings.Contains(stderr, "PRIVATE_WAIT") {
			t.Fatalf("failed verification returned identity/pause: %d %s %s", code, out, stderr)
		}
		if instances.Load() != 1 || pauses.Load() != spec.domainReads || unexpected.Load() != 0 || snapshot() != before {
			t.Fatal("failed doctor continued or mutated state")
		}
	}
	fault.Store(0)
	instances.Store(0)
	pauses.Store(0)
	before := snapshot()
	code, out, stderr := mutationInvoke(t, profilesPath, "missing", "doctor")
	if code != cli.ExitService || out != "" || !strings.Contains(stderr, `"code":"not_found"`) || instances.Load() != 1 || pauses.Load() != 1 || unexpected.Load() != 0 || snapshot() != before {
		t.Fatalf("pause verification did not reject a missing bound worker without extra reads: %d %s %s", code, out, stderr)
	}
	if f.resolverHit.Load() != 1 {
		t.Fatal("doctor contacted a provider")
	}
	// Configuration-only context remains a separate local command.
	code, _, stderr = run([]string{"--profiles-file", filepath.Join(t.TempDir(), "missing"), "--profile", "builder", "doctor"}, nil)
	if code != cli.ExitConfig || !strings.Contains(stderr, "profiles_unreadable") {
		t.Fatal("doctor lost profile failure")
	}
}
