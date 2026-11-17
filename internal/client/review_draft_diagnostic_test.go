package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func requireDraftDiagnostic(t *testing.T, hint *ReviewOperationDiagnostic) {
	t.Helper()
	if hint == nil || !strings.Contains(hint.Explanation, "GitHub Draft") || !strings.Contains(hint.Explanation, "initial review readiness") {
		t.Fatal("missing known Draft explanation", hint)
	}
	for _, part := range []string{"marking the PR ready", "remains authorized", "one fresh request", "new key", "current configuration/head", "old key replays", "Do not reset", "grant extra", "retry automatically"} {
		if !strings.Contains(hint.NextAction, part) {
			t.Fatal("unsafe Draft guidance", part, hint)
		}
	}
}
func TestAgentDraftDiagnosticPreservesTerminalOperationFacts(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		original := ReviewOperation{ID: "original_operation", ProjectID: "original_project", ActorID: "original_actor", WorkerID: "original_worker", Channel: channel, PullNumber: 42, State: "held", Reason: "pull_draft", Revision: 3, ConfigurationRevision: 5, HeadSHA: strings.Repeat("a", 40), CreatedAt: "original_start", UpdatedAt: "original_check"}
		full := AgentReviewOperation(original)
		requireDraftDiagnostic(t, full.Diagnostic)
		if !reflect.DeepEqual(full.ReviewOperation, original) {
			t.Fatal("retained operation changed")
		}
		compact := ReviewOperationStatus{ID: original.ID, Channel: channel, State: "held", Stage: "held", Reason: "pull_draft", Revision: 3, UpdatedAt: "original_check", NextAction: "read_diagnostics"}
		status := AgentReviewOperationStatus(compact)
		requireDraftDiagnostic(t, status.Diagnostic)
		if !reflect.DeepEqual(status.ReviewOperationStatus, compact) {
			t.Fatal("retained compact facts changed")
		}
		body, err := json.Marshal(full)
		if err != nil || len(body) > 2048 {
			t.Fatal("unbounded diagnostic", err)
		}
	}
}
func TestAgentDraftSelectionDiagnosticOnlyWithoutChild(t *testing.T) {
	for _, state := range []string{"held", "queued", "selecting", "selected", "empty"} {
		for _, child := range []bool{false, true} {
			value := ReviewRunState{Selection: ReviewRunSelection{ID: "original_selection", ProjectID: "original_project", Channel: "hosted", State: state, Reason: "pull_draft", Revision: 2}}
			if child {
				value.Selection.OperationID = "original_operation"
				value.Operation = &ReviewOperation{ID: "original_operation", Channel: "hosted", State: "active"}
			}
			result := AgentReviewSelection(value)
			if !reflect.DeepEqual(result.ReviewRunState, value) {
				t.Fatal("selection facts changed")
			}
			if state == "held" && !child {
				requireDraftDiagnostic(t, result.Diagnostic)
			} else if result.Diagnostic != nil {
				t.Fatal("pending or linked selection got replacement advice")
			}
		}
	}
}
func TestAgentDraftDiagnosticNeverInfersMissingSourceFacts(t *testing.T) {
	for _, reason := range []string{"", "pull_draft_unknown", "unknown", "PRIVATE_PROVIDER_BODY"} {
		if hint := agentReviewOperationDiagnostic("hosted", "held", reason); hint != nil {
			t.Fatal("Draft state was guessed", reason, hint)
		}
		if hint := selectionSourceDiagnostic(reason); hint != nil {
			t.Fatal("selection Draft state was guessed", reason, hint)
		}
	}
	for _, state := range []string{"queued", "preparing", "posting", "active", "uncertain", "completed", "failed"} {
		if hint := agentReviewOperationDiagnostic("hosted", state, "pull_draft"); hint != nil {
			t.Fatal("nonterminal state got a fresh-request hint", state)
		}
	}
	old := AgentReviewSelection(ReviewRunState{Selection: ReviewRunSelection{State: "held", Reason: "source_unavailable"}})
	if old.Diagnostic == nil || strings.Contains(old.Diagnostic.Explanation, "Draft") || !strings.Contains(old.Diagnostic.Explanation, "did not retain its exact failure cause") {
		t.Fatal("historical cause was invented")
	}
	pending := &ReviewPendingEvidence{Items: []ReviewPendingEvidenceItem{{OperationID: "original_operation", Reason: "source_unfinalized"}}}
	value := ReviewOperationStatus{Channel: "hosted", State: "held", Reason: "review_evidence_pending", NextAction: "read_diagnostics", PendingEvidence: pending}
	result := AgentReviewOperationStatus(value)
	if !reflect.DeepEqual(result.Diagnostic, pendingEvidenceDiagnostic(pending)) || result.NextAction != "resolve_pending_evidence" {
		t.Fatal("existing evidence priority changed")
	}
}

func TestQueuedCLIDraftDiagnosticKeepsOriginalRequest(t *testing.T) {
	hint := agentReviewOperationDiagnostic("cli", "queued", "cli_draft_wait")
	if hint == nil || !strings.Contains(hint.Explanation, "queued while the PR is Draft") || !strings.Contains(hint.NextAction, "Keep this request") {
		t.Fatal("Draft waiting guidance did not preserve the queued request", hint)
	}
	if hint := agentReviewOperationDiagnostic("hosted", "queued", "cli_draft_wait"); hint != nil {
		t.Fatal("Hosted got CLI-only draft guidance", hint)
	}
}
