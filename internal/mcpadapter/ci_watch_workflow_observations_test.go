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

func TestCIWatchWorkflowObservationsSDKAndCLIKeepGateAndProofLimit(t *testing.T) {
	t.Run("latest details", func(t *testing.T) { ciWatchWorkflowProjection(t, false) })
	t.Run("earlier execution attention", func(t *testing.T) { ciWatchWorkflowProjection(t, true) })
}

func ciWatchWorkflowProjection(t *testing.T, fallback bool) {
	identity := localContext()
	pin := client.CIWatchTarget{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), FailureWorkflowIDs: []int64{301}}
	response := client.CIWatchResult{ProjectID: identity.ProjectID, State: "pending", TimedOut: true, Targets: []client.CIWatchTargetResult{{
		PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: "pending", Reason: "ci_required_checks_pending", BaseRef: "feature", GateRef: "main", ObservedHeadSHA: pin.HeadSHA, ObservedBaseSHA: pin.BaseSHA, ObservedAt: "2026-11-01T00:00:00Z", CICommitSHA: pin.HeadSHA, Selection: "head", Required: 1, Pending: 1, Checks: []client.ReviewCIContext{{Context: "test", State: "pending", Reason: "ci_status_pending"}}, ObservationKey: strings.Repeat("c", 64), WorkflowObservations: []client.CIWorkflowObservation{{WorkflowID: 301, Basis: "latest_run_only", RunID: 401, RunAttempt: 2, Status: "completed", Conclusion: "success"}},
	}}}
	proofLimit := "latest run/current attempt"
	if fallback {
		response.State, response.TimedOut = "attention", false
		response.Targets[0].WorkflowObservations = nil
		response.Targets[0].WorkflowFailures = []client.CIWorkflowFailure{{WorkflowID: 301, RunID: 400, RunAttempt: 1, JobID: 501, CheckRunID: 601, Name: "Selected leaf", Conclusion: "failure"}}
		proofLimit = "explicitly skipped"
	}
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": identity.InstanceID})
			return
		}
		posts.Add(1)
		var body struct {
			Targets []client.CIWatchTarget `json:"targets"`
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("X-FC-CI-Watch") != "change-v1" || r.Header.Get("X-FC-CI-Workflow-Details") != "1" || r.Header.Get("X-FC-Worker") != identity.WorkerID || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Idempotency-Key") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, []client.CIWatchTarget{pin}) {
			t.Error("visibility changed read scope or workflow selection")
		}
		jsonResponse(w, response)
	}))
	defer service.Close()
	identity.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"chosen": identity}, credential(t))
	session := connect(t, adapter(t, path, "chosen"), "")
	var got client.CIWatchOutput
	decodeResult(t, call(t, session, "wait_for_ci", map[string]any{"targets": []client.CIWatchTarget{pin}, "wait_seconds": 0}), &got)
	if got.AllTargetsPassed || got.State != response.State || got.TimedOut != response.TimedOut || len(got.Targets) != 1 || !reflect.DeepEqual(got.Targets[0].CIWatchTargetResult, response.Targets[0]) || got.Targets[0].Diagnostic == nil || !strings.Contains(got.Targets[0].Diagnostic.Explanation, proofLimit) || !strings.Contains(got.Targets[0].Diagnostic.Explanation, "do not certify") || fallback && strings.Contains(got.Targets[0].Diagnostic.Explanation, "only the latest") {
		t.Fatal("SDK changed gate facts or omitted latest-only proof limit", got)
	}
	var out, diag bytes.Buffer
	args := []string{"--profiles-file", path, "--profile", "chosen", "reviews", "ci-wait", "--failure-workflow", "2:301", "--target", "2:" + pin.HeadSHA + ":" + pin.BaseSHA, "--wait-seconds", "0"}
	var cliValue client.CIWatchOutput
	if code := cli.RunContext(t.Context(), args, nil, &out, &diag); code != cli.ExitSuccess || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &cliValue) != nil || !reflect.DeepEqual(cliValue, got) {
		t.Fatal("CLI/SDK visibility differs", code, out.String(), diag.String())
	}
	if posts.Load() != 2 {
		t.Fatal("workflow visibility added requests", posts.Load())
	}
}
