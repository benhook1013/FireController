package mcpadapter_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestDoctorPauseSDKStdioUsesCurrentStateWithoutExtraRead(t *testing.T) {
	f := mutationFixture(t)
	pausePath := "/api/v1/projects/" + f.project + "/workers/" + f.sender + "/pause"
	var instances, pauses, unexpected atomic.Int64
	var fault atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			unexpected.Add(1)
		}
		switch r.URL.Path {
		case "/api/v1/instance":
			instances.Add(1)
			if fault.Load() == 1 {
				jsonResponse(w, map[string]string{"instance_id": "different-instance"})
				return
			}
		case pausePath, "/api/v1/projects/" + f.project + "/workers/worker-missing/pause":
			pauses.Add(1)
			if fault.Load() == 2 {
				jsonResponse(w, client.WorkerPause{WorkerID: "wrong-worker"})
				return
			}
		default:
			unexpected.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	binding := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, f.credential), "chosen")
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "doctor" {
			found = true
			if !strings.Contains(tool.Description, "worker_pause") || !strings.Contains(tool.Description, "private reason") {
				t.Fatal("doctor discovery hides current pause")
			}
		}
	}
	if !found {
		t.Fatal("doctor missing")
	}
	snapshot := func() string {
		t.Helper()
		state := apiRequest(t, f.handler, "GET", pausePath, f.admin, "", "", "", 200)
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
	for i, want := range []client.WorkerPause{{WorkerID: f.sender}, {WorkerID: f.sender, Revision: 1, Paused: true, Reason: reason}, {WorkerID: f.sender, Revision: 2, Paused: true}, {WorkerID: f.sender, Revision: 3}} {
		if i > 0 {
			raw, _ := json.Marshal(map[string]any{"expected_revision": i - 1, "paused": want.Paused, "reason": want.Reason})
			apiRequest(t, f.handler, "PUT", pausePath, f.admin, fmt.Sprintf("doctor-state-%d", i), f.sender, string(raw), 200)
		}
		before := snapshot()
		instances.Store(0)
		pauses.Store(0)
		result := call(t, session, "doctor", map[string]any{})
		var raw map[string]json.RawMessage
		decodeResult(t, result, &raw)
		var got struct {
			Verified    bool               `json:"verified"`
			WorkerID    string             `json:"worker_id"`
			WorkerPause client.WorkerPause `json:"worker_pause"`
		}
		decodeResult(t, result, &got)
		var pauseFields map[string]json.RawMessage
		if !got.Verified || got.WorkerID != f.sender || got.WorkerPause != want || len(raw) != 10 || json.Unmarshal(raw["worker_pause"], &pauseFields) != nil || len(pauseFields) != 4 {
			t.Fatalf("doctor state %d lost required fields: %v", i, result.StructuredContent)
		}
		for _, field := range []string{"profile", "server_url", "instance_id", "project_id", "worker_id", "verified", "client_version", "client_source_revision", "client_source_state"} {
			if len(raw[field]) == 0 {
				t.Fatalf("original doctor field %s missing", field)
			}
		}
		if instances.Load() != 1 || pauses.Load() != 1 || unexpected.Load() != 0 || snapshot() != before {
			t.Fatal("SDK doctor added reads or changed durable pause/history/receipts/events")
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
		result := call(t, session, "doctor", nil)
		diagnosticResult(t, result, spec.code, false)
		var failure map[string]json.RawMessage
		raw, _ := json.Marshal(result.StructuredContent)
		if json.Unmarshal(raw, &failure) != nil || failure["worker_pause"] != nil || failure["verified"] != nil || strings.Contains(string(raw), "PRIVATE_WAIT") {
			t.Fatal("failed verification returned pause")
		}
		if instances.Load() != 1 || pauses.Load() != spec.domainReads || unexpected.Load() != 0 || snapshot() != before {
			t.Fatal("failed SDK doctor continued or mutated state")
		}
	}
	fault.Store(0)
	instances.Store(0)
	pauses.Store(0)
	before := snapshot()
	missing := binding
	missing.Profile, missing.WorkerID = "missing", "worker-missing"
	missingSession := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"missing": missing}, f.credential), "missing")
	diagnosticResult(t, call(t, missingSession, "doctor", nil), "not_found", false)
	if instances.Load() != 1 || pauses.Load() != 1 || unexpected.Load() != 0 || snapshot() != before {
		t.Fatal("SDK pause verification did not reject a missing bound worker without extra reads")
	}
	if f.providers.Load() != 1 {
		t.Fatal("doctor contacted provider")
	}
}
