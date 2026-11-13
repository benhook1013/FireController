package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestCIWatchChangeSDKAndCLIKeepKnownVerdictsPending(t *testing.T) {
	for _, state := range []string{"failed", "passed"} {
		t.Run(state, func(t *testing.T) {
			testCIWatchKnownVerdict(t, state)
		})
	}
}

func testCIWatchKnownVerdict(t *testing.T, state string) {
	identity := localContext()
	pin := client.CIWatchTarget{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), AfterObservationKey: strings.Repeat("c", 64)}
	response := client.CIWatchResult{ProjectID: identity.ProjectID, State: "pending", TimedOut: true, Targets: []client.CIWatchTargetResult{{
		PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: "failed", Reason: "ci_required_checks_failed", BaseRef: "feature", GateRef: "main", ObservedHeadSHA: pin.HeadSHA, ObservedBaseSHA: pin.BaseSHA, ObservedAt: "2026-11-01T00:00:00Z", CICommitSHA: pin.HeadSHA, Selection: "head", Required: 1, Failed: 1,
		Checks: []client.ReviewCIContext{{Context: "build", State: "failed", Reason: "ci_failed", CheckRun: &client.ReviewCICheckRun{ID: 81, Status: "completed", Conclusion: "failure"}}}, ObservationKey: pin.AfterObservationKey, WaitingForChange: true,
	}}}
	if state == "passed" {
		item := &response.Targets[0]
		item.State, item.Reason, item.Failed, item.Satisfied = "passed", "", 0, 1
		item.Checks[0].State, item.Checks[0].Reason = "satisfied", ""
		item.Checks[0].CheckRun.Conclusion = "success"
	}
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FC-CI-Watch", "change-v3")
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": identity.InstanceID})
			return
		}
		posts.Add(1)
		var body struct {
			Targets []client.CIWatchTarget `json:"targets"`
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("X-FC-CI-Watch") != "change-v3" || r.Header.Get("X-FC-Worker") != identity.WorkerID || r.Header.Get("X-FC-Instance") != identity.InstanceID || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Idempotency-Key") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, []client.CIWatchTarget{pin}) {
			t.Error("SDK/CLI changed the known-failure read binding or baseline")
		}
		jsonResponse(w, response)
	}))
	defer service.Close()
	identity.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"chosen": identity}, credential(t))
	session := connect(t, adapter(t, path, "chosen"), "")
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name != "wait_for_ci" {
			continue
		}
		found = true
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil || !bytes.Contains(encoded, []byte(`"after_observation_key"`)) || !bytes.Contains(encoded, []byte(`^[a-f0-9]{64}$`)) || !bytes.Contains(encoded, []byte("passed or failed")) || !strings.Contains(tool.Description, "Pending checks alone") {
			t.Fatal("discovery does not advertise the exact optional baseline", string(encoded), err)
		}
	}
	if !found {
		t.Fatal("CI watch tool absent")
	}
	var got client.CIWatchOutput
	decodeResult(t, call(t, session, "wait_for_ci", map[string]any{"targets": []client.CIWatchTarget{pin}, "wait_seconds": 0}), &got)
	if got.State != "pending" || !got.TimedOut || got.AllTargetsPassed != (state == "passed") || len(got.Targets) != 1 || !reflect.DeepEqual(got.Targets[0].CIWatchTargetResult, response.Targets[0]) || got.Targets[0].Phase != "waiting_for_change" || got.Targets[0].Diagnostic == nil {
		t.Fatal("known failure was hidden or incorrectly credited", got)
	}
	var out, diag bytes.Buffer
	args := []string{"--profiles-file", path, "--profile", "chosen", "reviews", "ci-wait", "--after-observation", "2:" + pin.AfterObservationKey, "--target", "2:" + pin.HeadSHA + ":" + pin.BaseSHA, "--wait-seconds", "0"}
	var cliResult client.CIWatchOutput
	if code := cli.RunContext(t.Context(), args, nil, &out, &diag); code != cli.ExitSuccess || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &cliResult) != nil || !reflect.DeepEqual(cliResult, got) {
		t.Fatal("CLI and SDK disagree on the same known failure", code, out.String(), diag.String())
	}
	if posts.Load() != 2 {
		t.Fatal("known-failure read was automatically repeated", posts.Load())
	}
}

func TestCIWatchChangeSDKRejectsInvalidBaselineBeforeCredentials(t *testing.T) {
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reads.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": identity}, "missing-credential"), "chosen"), "")
	for _, value := range []any{nil, "", "short", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("c", 65), 123} {
		args := map[string]any{"targets": []map[string]any{{"pull_number": 2, "head_sha": strings.Repeat("a", 40), "base_sha": strings.Repeat("b", 40), "after_observation_key": value}}, "wait_seconds": 0}
		diagnosticResult(t, call(t, session, "wait_for_ci", args), "invalid_arguments", false)
	}
	for _, field := range []string{"AfterObservationKey", "private_body"} {
		args := map[string]any{"targets": []map[string]any{{"pull_number": 2, "head_sha": strings.Repeat("a", 40), field: strings.Repeat("c", 64)}}, "wait_seconds": 0}
		diagnosticResult(t, call(t, session, "wait_for_ci", args), "invalid_arguments", false)
	}
	// A valid key still requires an explicit base pin. Keep this refusal
	// separate so it cannot mask the malformed-key cases above.
	missingBase := map[string]any{"targets": []map[string]any{{"pull_number": 2, "head_sha": strings.Repeat("a", 40), "after_observation_key": strings.Repeat("c", 64)}}, "wait_seconds": 0}
	diagnosticResult(t, call(t, session, "wait_for_ci", missingBase), "invalid_arguments", false)
	if reads.Load() != 0 {
		t.Fatal("malformed baseline reached credentials/HTTP", reads.Load())
	}
}
