package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewCIMCPActualPublicIdentityAndProviderFreeRead(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.RawQuery != "" || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != f.sender {
			t.Error("CI SDK request lost profile binding")
		}
		if r.Method == "POST" && r.Header.Get("Prefer") != "wait=0" {
			t.Error("CI SDK immediate wait lost")
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	args := sourceMCPArguments()
	args["include_ci"] = true
	args["wait_seconds"] = 0
	providers := f.providers.Load()
	var receipt, replay, coalesced, source client.ReviewRequestReceipt
	result := call(t, session, "review_source_refresh", args)
	decodeResult(t, result, &receipt)
	if receipt.State != "queued" || receipt.Revision != 1 {
		t.Fatal("CI SDK fabricated terminal result", receipt)
	}
	data, _ := json.Marshal(result.StructuredContent)
	if len(data) > 1024 || strings.Contains(string(data), `"checks":`) {
		t.Fatal("CI receipt expanded inventories")
	}
	decodeResult(t, call(t, session, "review_source_refresh", args), &replay)
	if replay != receipt {
		t.Fatal("CI same-key replay changed")
	}
	args["include_ci"] = false
	diagnosticResult(t, call(t, session, "review_source_refresh", args), "idempotency_conflict", false)
	args["idempotency_key"] = "source-only"
	decodeResult(t, call(t, session, "review_source_refresh", args), &source)
	if source.ID == receipt.ID {
		t.Fatal("source-only coalesced CI work")
	}
	delete(args, "include_ci")
	args["idempotency_key"] = "omitted-is-source-only"
	decodeResult(t, call(t, session, "review_source_refresh", args), &coalesced)
	if coalesced != source {
		t.Fatal("explicit false differs from omitted CI option")
	}
	args["include_ci"] = true
	args["idempotency_key"] = "compatible-ci"
	decodeResult(t, call(t, session, "review_source_refresh", args), &coalesced)
	if coalesced != receipt {
		t.Fatal("compatible queued CI failed to coalesce")
	}
	var operation client.ReviewSourceOperation
	decodeResult(t, call(t, session, "review_source_refresh_read", map[string]any{"refresh_id": receipt.ID}), &operation)
	if !operation.IncludeCI || operation.CI != nil || operation.State != "queued" || operation.ProjectID != f.project {
		t.Fatal("CI stored operation lost scope/flag", operation)
	}
	var count, ciCount int
	if err := f.store.DB().QueryRow(`SELECT count(*),sum(include_ci) FROM review_source_refreshes`).Scan(&count, &ciCount); err != nil || count != 2 || ciCount != 1 {
		t.Fatal("SDK CI requests duplicated effects", count, ciCount, err)
	}
	var events, receipts int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM receipts)`).Scan(&events, &receipts); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	diagnosticResult(t, call(t, session, "review_source_ci_assess", map[string]any{"pull_number": 2}), "reviews_ci_unavailable", false)
	var afterEvents, afterReceipts int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM receipts)`).Scan(&afterEvents, &afterReceipts); err != nil || afterEvents != events || afterReceipts != receipts || requests.Load() != before+2 || f.providers.Load() != providers {
		t.Fatal("SDK local CI read mutated/refreshed/polled", err)
	}
}

// Controlled wire results exercise SDK/adapter decoding, not provider evidence.
func TestReviewCIMCPControlledDirectTerminalAndStoredAssessment(t *testing.T) {
	binding := localContext()
	target := client.ReviewSourceObservation{RepositoryID: 1, RepositoryName: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ParentSHA: strings.Repeat("b", 40), ObservedAt: "2026-10-16T00:00:00Z"}
	summary := client.ReviewSourceCISummary{State: "satisfied", Selection: "head", SelectedCommitSHA: target.HeadSHA, MergeState: "conflicting", ObservedAt: target.ObservedAt, Required: 1, Satisfied: 1}
	operation := client.ReviewSourceOperation{ID: mcpRefreshID, ProjectID: binding.ProjectID, PullNumber: 2, HeadSHA: target.HeadSHA, ConfigurationRevision: 1, State: "completed", Revision: 3, CreatedAt: target.ObservedAt, UpdatedAt: target.ObservedAt, Observation: &target, IncludeCI: true, CI: &summary}
	assessment := client.ReviewSourceCIAssessment{ProjectID: binding.ProjectID, RefreshID: mcpRefreshID, Source: "observed_ci", ConfigurationRevision: 1, SourceObservedAt: target.ObservedAt, EvaluatedAt: target.ObservedAt, Target: target, State: "satisfied", Selection: "head", SelectedCommitSHA: target.HeadSHA, MergeState: "conflicting", ObservedAt: target.ObservedAt, Required: 1, Satisfied: 1}
	var held atomic.Bool
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != binding.InstanceID || r.Header.Get("X-FC-Worker") != binding.WorkerID || r.URL.RawQuery != "" {
			t.Error("CI SDK controlled call lost scope")
		}
		base := "/api/v1/projects/" + binding.ProjectID + "/reviews/"
		switch r.URL.Path {
		case base + "source-refreshes":
			var input client.ReviewSourceRequest
			if json.NewDecoder(r.Body).Decode(&input) != nil || !input.IncludeCI || input.PullNumber != 2 || input.HeadSHA != target.HeadSHA || input.ExpectedConfigurationRevision != 1 || r.Method != "POST" || r.Header.Get("Prefer") != "wait=2" || r.Header.Get("Idempotency-Key") != "source-once" {
				t.Error("SDK default CI request/wait/key changed")
			}
			_ = json.NewEncoder(w).Encode(operation)
		case base + "source/2/ci":
			if r.Method != "GET" || r.Header.Get("Prefer") != "" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("SDK CI assessment used mutation")
			}
			value := assessment
			if held.Load() {
				value.State = "held"
				value.Reason = "source_comparison_changed"
			}
			_ = json.NewEncoder(w).Encode(value)
		default:
			t.Error("unexpected CI SDK route", r.URL.Path)
		}
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	args := sourceMCPArguments()
	args["include_ci"] = true
	var got client.ReviewSourceOperation
	result := call(t, session, "review_source_refresh", args)
	decodeResult(t, result, &got)
	if got.State != "completed" || !got.IncludeCI || got.CI == nil || got.CI.State != "satisfied" || got.Observation == nil {
		t.Fatal("SDK CI terminal result wrapped/lost", got)
	}
	for _, state := range []string{"satisfied", "held"} {
		held.Store(state == "held")
		result = call(t, session, "review_source_ci_assess", map[string]any{"pull_number": 2})
		var got client.ReviewSourceCIAssessment
		decodeResult(t, result, &got)
		data, _ := json.Marshal(result.StructuredContent)
		if got.State != state || got.Source != "observed_ci" || got.MergeState != "conflicting" || got.Target != target || len(data) > 16<<10 || strings.Contains(string(data), `"checks":`) {
			t.Fatal("SDK CI assessment lost as-of facts/compactness", got)
		}
	}
	if requests.Load() != 6 {
		t.Fatal("SDK CI tools polled", requests.Load())
	}
}

func TestReviewCIMCPInvalidBooleanAndReadSelectorsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-ci-credential"), "chosen")
	for _, value := range []any{nil, "true", 1, []any{}, map[string]any{}} {
		args := sourceMCPArguments()
		args["include_ci"] = value
		diagnosticResult(t, call(t, session, "review_source_refresh", args), "invalid_arguments", false)
	}
	args := sourceMCPArguments()
	data, _ := json.Marshal(args)
	duplicate := strings.TrimSuffix(string(data), "}") + `,"include_ci":true,"include_ci":false}`
	diagnosticResult(t, call(t, session, "review_source_refresh", json.RawMessage(duplicate)), "invalid_arguments", false)
	for _, args := range []any{nil, map[string]any{}, map[string]any{"pull_number": nil}, map[string]any{"pull_number": 0}, map[string]any{"pull_number": "2"}, map[string]any{"pull_number": 2, "include_ci": true}, map[string]any{"pull_number": 2, "refresh": true}, map[string]any{"pull_number": 2, "project_id": "other"}} {
		diagnosticResult(t, call(t, session, "review_source_ci_assess", args), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid CI SDK arguments reached credentials/HTTP")
	}
}
