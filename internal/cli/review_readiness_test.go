package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewReadinessCLIActualOptionsReplayAndLocalRead(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+f.admin || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.RawQuery != "" || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != worker {
			t.Error("readiness CLI lost binding")
		}
		if r.Method == "POST" && r.Header.Get("Prefer") != "wait=0" {
			t.Error("immediate wait lost")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	providers := f.resolverHit.Load()
	ids := map[string]bool{}
	for _, option := range []struct {
		key           string
		ci, readiness bool
	}{{"source", false, false}, {"ci", true, false}, {"readiness", false, true}, {"both", true, true}} {
		args := sourceCLIArgs()
		args[len(args)-1] = option.key
		args = append(args, "--wait-seconds", "0")
		if option.ci {
			args = append(args, "--ci")
		}
		if option.readiness {
			args = append(args, "--readiness")
		}
		receipt := mutationSuccess(t, path, "builder", args...)
		id, _ := receipt["id"].(string)
		if len(receipt) != 3 || id == "" || ids[id] || receipt["state"] != "queued" {
			t.Fatal("options coalesced across distinct scope", receipt)
		}
		ids[id] = true
		if replay := mutationSuccess(t, path, "builder", args...); encodeString(replay) != encodeString(receipt) {
			t.Fatal("same-key replay changed", replay)
		}
		compatible := append([]string(nil), args...)
		for i := range compatible {
			if compatible[i] == option.key {
				compatible[i] = "compatible-" + option.key
			}
		}
		if got := mutationSuccess(t, path, "builder", compatible...); encodeString(got) != encodeString(receipt) {
			t.Fatal("compatible scope failed coalescing", got)
		}
		code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "source-operation", id)
		var operation client.ReviewSourceOperation
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &operation) != nil || operation.IncludeCI != option.ci || operation.IncludeReadiness != option.readiness || operation.State != "queued" || operation.CI != nil || operation.ReviewState != nil || operation.ProjectID != project {
			t.Fatal("operation changed requested booleans or invented observations", code, out, stderr)
		}
		if option.key == "source" {
			code, out, stderr = mutationInvoke(t, path, "builder", append(args, "--readiness")...)
			if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "idempotency_conflict") || strings.Contains(stderr, "outcome_unknown") {
				t.Fatal("readiness did not bind key identity", code, stderr)
			}
		}
	}
	var count, ciCount, readyCount int
	if err := f.store.DB().QueryRow(`SELECT count(*),sum(include_ci),sum(include_readiness) FROM review_source_refreshes`).Scan(&count, &ciCount, &readyCount); err != nil || count != 4 || ciCount != 2 || readyCount != 2 {
		t.Fatal("replay/coalescing duplicated effects", count, ciCount, readyCount, err)
	}
	var events, receipts int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM receipts)`).Scan(&events, &receipts); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "readiness", "--pr", "2")
	var afterEvents, afterReceipts int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM receipts)`).Scan(&afterEvents, &afterReceipts); err != nil || code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "reviews_readiness_unavailable") || requests.Load() != before+2 || afterEvents != events || afterReceipts != receipts || f.resolverHit.Load() != providers {
		t.Fatal("unavailable readiness read performed work or invented result", code, stderr, err)
	}
}

func cliReadinessFixture(project string) client.ReviewSourceReadinessAssessment {
	target := cliSourceAssessment(project).Target
	return client.ReviewSourceReadinessAssessment{ProjectID: project, RefreshID: cliRefreshID, Source: "observed_comparison", ConfigurationRevision: 1, PolicyRevision: 1, SourceObservedAt: target.ObservedAt, EvaluatedAt: target.ObservedAt, Target: target, State: "ready_as_observed", Reasons: []string{}, Channels: []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}, CI: client.ReviewSourceCISummary{State: "satisfied", Selection: "test_merge", SelectedCommitSHA: strings.Repeat("c", 40), MergeState: "verified", ObservedAt: target.ObservedAt, Required: 1, Satisfied: 1}, ReviewState: client.ReviewSourceReviewStateSummary{State: "complete", ObservedAt: target.ObservedAt, PullState: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", TotalThreads: 3}}
}

// Controlled compact wire facts verify the adapter, not provider or merge gates.
func TestReviewReadinessCLIControlledResultsAndWaitPreferences(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "ready-wire")
	worker := f.worker(t, project, "Builder")
	assessment := cliReadinessFixture(project)
	operation := cliSourceOperation(project)
	operation.State = "completed"
	operation.Reason = ""
	operation.IncludeReadiness = true
	operation.Observation = &assessment.Target
	operation.CI = &assessment.CI
	operation.ReviewState = &assessment.ReviewState
	var mode atomic.Int64
	operationAtMode := func() client.ReviewSourceOperation {
		value := operation
		if mode.Load() == 3 {
			ci := client.ReviewSourceCISummary{State: "unproven", Reason: "ci_provider_observation_changed_assessment", ObservedAt: assessment.Target.ObservedAt}
			value.CI = &ci
		}
		return value
	}
	var requests atomic.Int64
	var expectedWait atomic.Int64
	expectedWait.Store(2)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.admin || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != worker || r.URL.RawQuery != "" {
			t.Error("readiness wire call lost scope")
		}
		base := "/api/v1/projects/" + project + "/reviews/"
		switch r.URL.Path {
		case base + "source-refreshes":
			var input client.ReviewSourceRequest
			if json.NewDecoder(r.Body).Decode(&input) != nil || !input.IncludeReadiness || input.IncludeCI || input.PullNumber != 2 || input.ExpectedConfigurationRevision != 1 || input.HeadSHA != assessment.Target.HeadSHA || r.Method != "POST" || r.Header.Get("Idempotency-Key") != "explicit-source" || r.Header.Get("Prefer") != "wait="+strconv.FormatInt(expectedWait.Load(), 10) {
				t.Error("readiness/default CI flag/wait changed")
			}
			if expectedWait.Load() == 0 {
				w.WriteHeader(202)
				_ = json.NewEncoder(w).Encode(client.ReviewRequestReceipt{ID: cliRefreshID, State: "queued", Revision: 1})
			} else {
				_ = json.NewEncoder(w).Encode(operationAtMode())
			}
		case base + "source-refreshes/" + cliRefreshID:
			_ = json.NewEncoder(w).Encode(operationAtMode())
		case base + "source/2/readiness":
			if r.Method != "GET" || r.Header.Get("Prefer") != "" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("readiness read mutated or refreshed")
			}
			value := assessment
			switch mode.Load() {
			case 1:
				value.State = "held"
				value.Reasons = []string{"unresolved_outdated_threads"}
				value.ReviewState.UnresolvedOutdated = 1
			case 2:
				value.State = "held"
				value.Reasons = []string{"ci_merge_state_unproven"}
				value.CI = client.ReviewSourceCISummary{State: "unproven", Reason: "ci_candidate_unproven", MergeState: "unproven", ObservedAt: value.Target.ObservedAt}
			case 3:
				value.State = "held"
				value.Reasons = []string{"ci_unproven"}
				value.CI = *operationAtMode().CI
			}
			_ = json.NewEncoder(w).Encode(value)
		default:
			t.Error("unexpected readiness route", r.URL.Path)
		}
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	providers := f.resolverHit.Load()
	for _, wait := range []int{-1, 0, 10} {
		args := append(sourceCLIArgs(), "--readiness")
		if wait < 0 {
			expectedWait.Store(2)
		} else {
			expectedWait.Store(int64(wait))
			args = append(args, "--wait-seconds", strconv.Itoa(wait))
		}
		code, out, stderr := mutationInvoke(t, path, "builder", args...)
		if code != 0 || stderr != "" || len(out) > 16<<10 || strings.Count(out, "\n") != 1 || strings.Contains(out, `"operation":`) || strings.Contains(out, `"threads":`) || strings.Contains(out, `"checks":`) {
			t.Fatal("readiness result expanded/wrapped", code, out, stderr)
		}
		if wait == 0 {
			var got client.ReviewRequestReceipt
			if json.Unmarshal([]byte(out), &got) != nil || got.State != "queued" {
				t.Fatal(out)
			}
		} else {
			var got client.ReviewSourceOperation
			if json.Unmarshal([]byte(out), &got) != nil || !got.IncludeReadiness || got.IncludeCI || got.CI == nil || got.ReviewState == nil || got.State != "completed" {
				t.Fatal(out)
			}
		}
	}
	for i := int64(0); i < 4; i++ {
		mode.Store(i)
		code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "readiness", "--pr", "2")
		var got client.ReviewReadinessOutput
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || got.Target != assessment.Target || got.Source != "observed_comparison" || len(out) > 16<<10 || strings.Contains(out, `"threads":`) || strings.Contains(out, `"checks":`) || (got.State == "ready_as_observed") != (i == 0) {
			t.Fatal("readiness lost held/observed facts", code, out, stderr)
		}
		if (got.CIDiagnostic != nil) != (i == 3) {
			t.Fatal("readiness diagnostic missing or spurious", got)
		}
		if i == 1 && got.ReviewState.UnresolvedOutdated != 1 || i == 2 && got.CI.MergeState != "unproven" {
			t.Fatal(got)
		}
	}
	mode.Store(3)
	expectedWait.Store(2)
	for _, args := range [][]string{append(sourceCLIArgs(), "--readiness"), {"reviews", "source-operation", cliRefreshID}} {
		code, out, stderr := mutationInvoke(t, path, "builder", args...)
		var got client.ReviewSourceOperationOutput
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || got.CIDiagnostic == nil || got.CI == nil || got.CI.State != "unproven" || got.CI.Required != 0 || got.ID != operation.ID || got.HeadSHA != operation.HeadSHA || got.State != "completed" {
			t.Fatal("source result lost diagnostic or original facts", code, out, stderr)
		}
	}
	if requests.Load() != 18 || f.resolverHit.Load() != providers {
		t.Fatal("readiness adapter polled or used provider", requests.Load())
	}
}

func TestReviewReadinessCLIInvalidOptionsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	path := fixture(t, encode(t, map[string]any{"version": 1, "profiles": map[string]any{"chosen": map[string]any{"server_url": service.URL, "instance_id": "instance-fixture", "project_id": "project-fixture", "worker_id": "worker-fixture", "credential_file": "missing-ready-token"}}}))
	cases := [][]string{{"reviews", "readiness"}, {"reviews", "readiness", "--pr", "02"}, {"reviews", "readiness", "--pr", "2", "--readiness"}, {"reviews", "readiness", "--pr", "2", "--refresh"}, {"reviews", "readiness", "--pr", "2", "--project", "other"}, {"reviews", "readiness", "--pr", "2", "--pr", "2"}}
	for _, suffix := range [][]string{{"--readiness=true"}, {"--readiness=false"}, {"--readiness=null"}, {"--readiness=1"}, {"--readiness="}, {"--readiness", "--readiness"}, {"--readiness", "true"}, {"--Readiness"}, {"--include-readiness"}, {"--readiness", "--wait-seconds", "61"}} {
		cases = append(cases, append(sourceCLIArgs(), suffix...))
	}
	for _, args := range cases {
		code, out, stderr := mutationInvoke(t, path, "chosen", args...)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("invalid readiness args reached credentials", args, code, stderr)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid readiness args reached HTTP")
	}
}
