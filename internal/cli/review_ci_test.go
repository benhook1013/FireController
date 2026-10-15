package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewCICLIPublicRefreshIdentityAndLocalUnavailable(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+f.admin || r.URL.RawQuery != "" || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != worker {
			t.Error("CI CLI lost selected profile binding")
		}
		if r.Method == "POST" && r.Header.Get("Prefer") != "wait=0" {
			t.Error("immediate CI wait lost")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	source := append(sourceCLIArgs(), "--wait-seconds", "0")
	providers := f.resolverHit.Load()
	original := mutationSuccess(t, path, "builder", source...)
	ci := append(append([]string{}, source...), "--ci")
	code, out, stderr := mutationInvoke(t, path, "builder", ci...)
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "idempotency_conflict") || strings.Contains(stderr, "outcome_unknown") {
		t.Fatal("CI flag changed same-key action identity", code, stderr)
	}
	for i := range ci {
		if ci[i] == "explicit-source" {
			ci[i] = "explicit-ci"
		}
	}
	receipt := mutationSuccess(t, path, "builder", ci...)
	if receipt["id"] == original["id"] || receipt["state"] != "queued" || receipt["revision"] != float64(1) {
		t.Fatal("CI request coalesced source-only work", receipt)
	}
	replay := mutationSuccess(t, path, "builder", ci...)
	if encodeString(replay) != encodeString(receipt) {
		t.Fatal("CI same-key receipt changed")
	}
	for i := range ci {
		if ci[i] == "explicit-ci" {
			ci[i] = "compatible-ci"
		}
	}
	compatible := mutationSuccess(t, path, "builder", ci...)
	if encodeString(compatible) != encodeString(receipt) {
		t.Fatal("compatible queued CI work did not coalesce")
	}
	var operation client.ReviewSourceOperation
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "source-operation", receipt["id"].(string))
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &operation) != nil || !operation.IncludeCI || operation.CI != nil || operation.State != "queued" || operation.ProjectID != project {
		t.Fatal("CI operation lost identity or fabricated inventory", code, out, stderr)
	}
	var count, withCI int
	if err := f.store.DB().QueryRow(`SELECT count(*),sum(include_ci) FROM review_source_refreshes`).Scan(&count, &withCI); err != nil || count != 2 || withCI != 1 {
		t.Fatal("CI/source action effects duplicated", count, withCI, err)
	}
	before := requests.Load()
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "ci-assess", "--pr", "2")
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "reviews_ci_unavailable") || requests.Load() != before+2 || f.resolverHit.Load() != providers {
		t.Fatal("local CI read refreshed/polled or invented success", code, stderr)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_source_refreshes`).Scan(&count); err != nil || count != 2 {
		t.Fatal("local unavailable assessment enqueued work", count, err)
	}
}

// Controlled HTTP result shapes test the adapters, not provider CI correctness.
func TestReviewCICLIControlledTerminalAndAssessmentAreCompact(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "ci-wire")
	worker := f.worker(t, project, "Builder")
	target := cliSourceAssessment(project).Target
	summary := client.ReviewSourceCISummary{State: "satisfied", Selection: "head", SelectedCommitSHA: target.HeadSHA, MergeState: "conflicting", ObservedAt: target.ObservedAt, Required: 1, Satisfied: 1}
	operation := cliSourceOperation(project)
	operation.State = "completed"
	operation.Reason = ""
	operation.IncludeCI = true
	operation.Observation = &target
	operation.CI = &summary
	assessment := client.ReviewSourceCIAssessment{ProjectID: project, RefreshID: cliRefreshID, Source: "observed_ci", ConfigurationRevision: 1, SourceObservedAt: target.ObservedAt, EvaluatedAt: target.ObservedAt, Target: target, State: summary.State, Selection: summary.Selection, SelectedCommitSHA: summary.SelectedCommitSHA, MergeState: summary.MergeState, ObservedAt: summary.ObservedAt, Required: 1, Satisfied: 1}
	var requests atomic.Int64
	var held atomic.Bool
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+f.admin || r.URL.RawQuery != "" {
			t.Error("controlled CI read lost profile")
		}
		base := "/api/v1/projects/" + project + "/reviews/"
		switch r.URL.Path {
		case base + "source-refreshes":
			var request client.ReviewSourceRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil || !request.IncludeCI || request.PullNumber != 2 || request.HeadSHA != target.HeadSHA || request.ExpectedConfigurationRevision != 1 || r.Method != "POST" || r.Header.Get("Prefer") != "wait=2" || r.Header.Get("Idempotency-Key") != "explicit-source" {
				t.Error("CI default wait/request changed")
			}
			_ = json.NewEncoder(w).Encode(operation)
		case base + "source/2/ci":
			if r.Method != "GET" || r.Header.Get("Prefer") != "" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("CI assessment performed mutation")
			}
			value := assessment
			if held.Load() {
				value.State, value.Reason = "held", "source_configuration_changed"
			}
			_ = json.NewEncoder(w).Encode(value)
		default:
			t.Error("unexpected CI route", r.URL.Path)
		}
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	code, out, stderr := mutationInvoke(t, path, "builder", append(sourceCLIArgs(), "--ci")...)
	var got client.ReviewSourceOperation
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || got.State != "completed" || !got.IncludeCI || got.CI == nil || got.CI.State != "satisfied" || len(out) > 16<<10 || strings.Count(out, "\n") != 1 || strings.Contains(out, `"operation":`) {
		t.Fatal("CI terminal result expanded/wrapped", code, out, stderr)
	}
	for _, state := range []string{"satisfied", "held"} {
		held.Store(state == "held")
		code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "ci-assess", "--pr", "2")
		var got client.ReviewSourceCIAssessment
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || got.State != state || got.Source != "observed_ci" || got.MergeState != "conflicting" || got.Target != target || len(out) > 16<<10 || strings.Contains(out, `"checks":`) {
			t.Fatal("CI local assessment lost as-of identity/compactness", code, out, stderr)
		}
	}
	if requests.Load() != 6 {
		t.Fatal("CI adapters polled", requests.Load())
	}
}

func TestReviewCICLIInvalidOptionsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	path := fixture(t, encode(t, map[string]any{"version": 1, "profiles": map[string]any{"chosen": map[string]any{"server_url": service.URL, "instance_id": "instance-fixture", "project_id": "project-fixture", "worker_id": "worker-fixture", "credential_file": "missing-ci-token"}}}))
	cases := [][]string{{"reviews", "ci-assess"}, {"reviews", "ci-assess", "--pr", "02"}, {"reviews", "ci-assess", "--pr", "2", "--ci"}, {"reviews", "ci-assess", "--pr", "2", "--refresh"}, {"reviews", "ci-assess", "--pr", "2", "--project", "other"}}
	for _, suffix := range [][]string{{"--ci=true"}, {"--ci=false"}, {"--ci="}, {"--ci", "--ci"}, {"--ci", "true"}} {
		cases = append(cases, append(sourceCLIArgs(), suffix...))
	}
	for _, args := range cases {
		code, out, stderr := mutationInvoke(t, path, "chosen", args...)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("invalid CI args reached credentials", args, code, stderr)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid CI args reached HTTP")
	}
}
