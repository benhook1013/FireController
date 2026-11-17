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

func TestCIWatchWorkflowFailuresSDKAndCLIAttentionKeepsGatePending(t *testing.T) {
	identity := localContext()
	pin := client.CIWatchTarget{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), FailureWorkflowIDs: []int64{301}}
	response := client.CIWatchResult{ProjectID: identity.ProjectID, State: "attention", Targets: []client.CIWatchTargetResult{{
		PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: "pending", Reason: "ci_required_checks_pending", BaseRef: "feature", GateRef: "main", ObservedHeadSHA: pin.HeadSHA, ObservedBaseSHA: pin.BaseSHA, ObservedAt: "2026-11-01T00:00:00Z", CICommitSHA: pin.HeadSHA, Selection: "head", Required: 1, Pending: 1,
		Checks: []client.ReviewCIContext{{Context: "test", State: "pending", Reason: "ci_status_pending"}}, ObservationKey: strings.Repeat("c", 64),
		WorkflowFailures: []client.CIWorkflowFailure{{WorkflowID: 301, RunID: 401, RunAttempt: 2, JobID: 501, CheckRunID: 601, Name: "unit tests", Conclusion: "failure"}},
	}}}
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
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("X-FC-CI-Watch") != "change-v3" || r.Header.Get("X-FC-Worker") != identity.WorkerID || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Idempotency-Key") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, []client.CIWatchTarget{pin}) {
			t.Error("workflow attention changed the exact read selection or authority")
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
		var schema struct {
			Properties map[string]struct {
				Items struct {
					Properties map[string]struct {
						Type        string `json:"type"`
						MinItems    int    `json:"minItems"`
						MaxItems    int    `json:"maxItems"`
						UniqueItems bool   `json:"uniqueItems"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"properties"`
		}
		if err != nil || json.Unmarshal(encoded, &schema) != nil {
			t.Fatal("invalid discovery schema", err)
		}
		ids := schema.Properties["targets"].Items.Properties["failure_workflow_ids"]
		if ids.Type != "array" || ids.MinItems != 1 || ids.MaxItems != 16 || !ids.UniqueItems {
			t.Fatal("workflow selection bounds missing from SDK discovery", string(encoded))
		}
	}
	if !found {
		t.Fatal("CI watch tool missing")
	}
	var got client.CIWatchOutput
	decodeResult(t, call(t, session, "wait_for_ci", map[string]any{"targets": []client.CIWatchTarget{pin}, "wait_seconds": 0}), &got)
	if got.State != "attention" || got.AllTargetsPassed || len(got.Targets) != 1 || got.Targets[0].State != "pending" || got.Targets[0].Failed != 0 || got.Targets[0].Pending != 1 || !reflect.DeepEqual(got.Targets[0].CIWatchTargetResult, response.Targets[0]) || got.Targets[0].Diagnostic == nil {
		t.Fatal("SDK confused workflow attention with the required gate", got)
	}
	var out, diag bytes.Buffer
	args := []string{"--profiles-file", path, "--profile", "chosen", "reviews", "ci-wait", "--failure-workflow", "2:301", "--target", "2:" + pin.HeadSHA + ":" + pin.BaseSHA, "--wait-seconds", "0"}
	var cliResult client.CIWatchOutput
	if code := cli.RunContext(t.Context(), args, nil, &out, &diag); code != cli.ExitSuccess || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &cliResult) != nil || !reflect.DeepEqual(cliResult, got) {
		t.Fatal("CLI/SDK workflow attention differs", code, out.String(), diag.String())
	}
	if posts.Load() != 2 {
		t.Fatal("workflow attention triggered extra requests", posts.Load())
	}
}

func TestCIWatchWorkflowFailuresSDKRejectsInvalidSelectionsBeforeCredentials(t *testing.T) {
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reads.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": identity}, "missing-credential"), "chosen"), "")
	tooMany := make([]int64, 17)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	for _, ids := range []any{nil, []int64{}, []int64{0}, []int64{-1}, []int64{301, 301}, tooMany, []any{"301"}, []any{nil}, []any{1.5}} {
		args := map[string]any{"targets": []map[string]any{{"pull_number": 2, "head_sha": strings.Repeat("a", 40), "failure_workflow_ids": ids}}, "wait_seconds": 0}
		diagnosticResult(t, call(t, session, "wait_for_ci", args), "invalid_arguments", false)
	}
	if reads.Load() != 0 {
		t.Fatal("invalid workflow selection reached credentials/HTTP", reads.Load())
	}
}
