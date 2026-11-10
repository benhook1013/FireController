package mcpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestSchedulingSDKBoundControlsAndDiscovery(t *testing.T) {
	f := mutationFixture(t)
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "scheduling-fixture", Provider: "synthetic", Account: "fixture", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	engine.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	managerCredential := filepath.Join(t.TempDir(), "manager.token")
	if err := os.WriteFile(managerCredential, []byte(f.admin), 0600); err != nil {
		t.Fatal(err)
	}
	managerProfile := profiles(t, map[string]profile.Context{"chosen": {Profile: "chosen", ServerURL: server.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}}, managerCredential)
	manager := reviewControlsStdio(t, managerProfile, "chosen")
	writer := connect(t, f.adapter(t, server.URL, f.sender), "")
	beforeProviders := f.providers.Load()
	var initial client.ReviewScheduling
	decodeResult(t, call(t, writer, "review_scheduling_read", nil), &initial)
	if initial.Revision != 0 || initial.AutomaticHosted || initial.PausedPulls == nil {
		t.Fatal(initial)
	}
	diagnosticResult(t, call(t, writer, "review_scheduling_configure", map[string]any{"expected_revision": 0, "automatic_hosted": true, "idempotency_key": "writer-cannot-enable"}), "forbidden", false)
	input := map[string]any{"expected_revision": 0, "automatic_hosted": true, "idempotency_key": "manager-enable"}
	var first, replay client.ReviewRequestReceipt
	decodeResult(t, call(t, manager, "review_scheduling_configure", input), &first)
	decodeResult(t, call(t, manager, "review_scheduling_configure", input), &replay)
	if first != replay || first.ID != f.project || first.Revision != 1 {
		t.Fatal(first, replay)
	}
	// CLI opt-in is independent; a legacy Hosted-only write preserves it.
	decodeResult(t, call(t, manager, "review_scheduling_configure", map[string]any{"expected_revision": 1, "automatic_hosted": false, "automatic_cli": true, "idempotency_key": "manager-cli-enable"}), &first)
	var automatic client.ReviewScheduling
	decodeResult(t, call(t, writer, "review_scheduling_read", nil), &automatic)
	if automatic.AutomaticHosted || !automatic.AutomaticCLI || automatic.AuthorizingWorkerID != f.sender {
		t.Fatal("CLI opt-in lost shared authorization", automatic)
	}
	decodeResult(t, call(t, manager, "review_scheduling_configure", map[string]any{"expected_revision": 2, "automatic_hosted": true, "idempotency_key": "manager-legacy-preserve"}), &first)
	decodeResult(t, call(t, writer, "review_scheduling_read", nil), &automatic)
	if !automatic.AutomaticHosted || !automatic.AutomaticCLI {
		t.Fatal("legacy configuration changed CLI opt-in", automatic)
	}
	decodeResult(t, call(t, manager, "review_scheduling_configure", map[string]any{"expected_revision": 3, "automatic_hosted": true, "automatic_cli": false, "idempotency_key": "manager-cli-disable"}), &first)
	decodeResult(t, call(t, writer, "review_scheduling_read", nil), &automatic)
	if !automatic.AutomaticHosted || automatic.AutomaticCLI {
		t.Fatal("explicit CLI disable was lost", automatic)
	}
	var paused client.ReviewRequestReceipt
	decodeResult(t, call(t, writer, "review_pull_scheduling_update", map[string]any{"pull_number": 7, "expected_revision": 0, "paused": true, "reason": "Preparing changes", "idempotency_key": "pause-before-intent"}), &paused)
	if paused.ID != strconv.Itoa(7) || paused.Revision != 1 {
		t.Fatal(paused)
	}
	var state client.ReviewPullScheduling
	decodeResult(t, call(t, writer, "review_pull_scheduling_read", map[string]any{"pull_number": 7}), &state)
	if !state.Paused || state.Reason != "Preparing changes" || state.ProjectID != f.project {
		t.Fatal(state)
	}
	decodeResult(t, call(t, writer, "review_pull_scheduling_update", map[string]any{"pull_number": 7, "expected_revision": 1, "paused": false, "idempotency_key": "resume-before-intent"}), &paused)
	decodeResult(t, call(t, writer, "review_pull_scheduling_read", map[string]any{"pull_number": 7}), &state)
	if state.Paused || state.Reason != "" || state.Revision != 2 {
		t.Fatal(state)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"review_scheduling_read", map[string]any{"project_id": "foreign"}},
		{"review_scheduling_configure", map[string]any{"expected_revision": 1, "automatic_hosted": nil, "idempotency_key": "null"}},
		{"review_scheduling_configure", map[string]any{"expected_revision": 1, "idempotency_key": "missing-bool"}},
		{"review_pull_scheduling_read", map[string]any{"pull_number": 0}},
		{"review_pull_scheduling_update", map[string]any{"pull_number": 7, "expected_revision": 2, "paused": nil, "idempotency_key": "null-pause"}},
		{"review_pull_scheduling_update", map[string]any{"pull_number": 7, "expected_revision": 2, "paused": true, "reason": nil, "idempotency_key": "null-reason"}},
	} {
		diagnosticResult(t, call(t, manager, tc.name, tc.args), "invalid_arguments", false)
	}
	listed, err := allDiscoveredTools(t, manager)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tool := range listed.Tools {
		if tool.Name != "review_scheduling_read" && tool.Name != "review_scheduling_configure" && tool.Name != "review_pull_scheduling_read" && tool.Name != "review_pull_scheduling_update" {
			continue
		}
		seen++
		write := tool.Name == "review_scheduling_configure" || tool.Name == "review_pull_scheduling_update"
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint == write || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint != (tool.Name == "review_scheduling_configure") {
			t.Fatal("unsafe annotations", tool.Name)
		}
	}
	if seen != 4 || f.providers.Load() != beforeProviders {
		t.Fatal("missing tools or provider work", seen, f.providers.Load(), beforeProviders)
	}
	for _, table := range []string{"review_selections", "review_operations", "review_execution_attempts"} {
		var n int
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("handler started review work", table, n, err)
		}
	}
}
