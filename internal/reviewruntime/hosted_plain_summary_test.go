package reviewruntime

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestRetainHostedPlainStandingSummarySameAttemptAndFinalization(t *testing.T) {
	f, record := runtimeStandingFixture(t, nil, nil)
	ack := &record.Snapshot.IssueComments[2]
	ack.Body = "<!-- coderabbit-ai-generated:v1 -->\n\n" + strings.Replace(strings.Replace(ack.Body, "✅ Action performed", "Action performed", 1), "Full review finished.", "<dl>\n<dd>\n\nFull review finished.\n\n</dd>\n</dl>", 1)
	original := f.a
	journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, original.ID)
	if err != nil || len(journal) != 1 || journal[0].Input.Kind != "allowance_query" {
		t.Fatal("complete original pre-POST baseline unavailable", err)
	}
	baseline := journal[0].Input
	baselineRef := CaptureReference{ID: baseline.CaptureID, SHA256: baseline.CaptureSHA256, Bytes: baseline.CaptureBytes}
	baselineRaw := hostedBridgeBytes(t, f, baselineRef)
	baselineRecord, err := f.captures.Read(t.Context(), original.ID, baselineRef)
	if err != nil || !baselineRecord.Snapshot.Complete {
		t.Fatal("baseline is not a complete source collection", err)
	}
	originalState := cliAttemptState(t, f.f.store, original.ID)
	originalBody, _ := json.Marshal(record)
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, original, record)
	if err != nil || !result.Complete || result.Reason != "capture_recorded" {
		t.Fatal("plain summary did not retain standing zero", result, err)
	}
	evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), result.Receipt.ID)
	if err != nil || evidence.AttemptID != original.ID || evidence.OperationID != original.OperationID || evidence.Anchor != *original.Anchor {
		t.Fatal("original native binding changed", err)
	}
	h := evidence.Capture.Hosted
	if h == nil || h.StandingSummary == nil || h.StandingSummary.CommentID != 501 || h.StandingSummary.ProviderRunID != runtimeStandingRun || h.TriggerCommentID != 100 || len(h.ReviewIDs) != 0 || h.PublicationID != "" || h.ProviderAttemptID != "" || evidence.Capture.Review.ReportedFindings != 0 || len(evidence.Capture.Review.Findings) != 0 || !evidence.Capture.Review.Complete {
		t.Fatal("standing zero identity/counts changed or invented")
	}
	retainedRaw, err := f.captures.Read(t.Context(), original.ID, result.Capture)
	if err != nil || !reflect.DeepEqual(retainedRaw, record) || cliAttemptState(t, f.f.store, original.ID) != originalState {
		t.Fatal("source body changed or retention settled original attempt", err)
	}
	zero := int64(0)
	if _, err = f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "plain-premature", result.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err == nil {
		t.Fatal("active capture finalized")
	}
	if recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 {
		t.Fatal("retention granted round credit")
	}
	completed, err := f.f.engine.Advance(t.Context(), f.f.project, original.ID, original.Revision, "completed", "synthetic_complete", original.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	adjudication, err := f.f.engine.ReadReviewAdjudication(t.Context(), f.f.scope(t, f.f.writer, false), result.Receipt.ID)
	if err != nil || adjudication.Finalized || recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 {
		t.Fatal("completion silently finalized or credited source", err)
	}
	finalized, err := f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "plain-finalize", result.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		again, err := RetainHosted(t.Context(), f.captures, f.f.engine, completed, record)
		if err != nil || again != result {
			t.Fatal("exact original recovery replay changed result", err)
		}
		replay, err := f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "plain-finalize", result.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero})
		if err != nil || !bytes.Equal(replay, finalized) {
			t.Fatal("finalization replay changed receipt", err)
		}
	}
	adjudication, err = f.f.engine.ReadReviewAdjudication(t.Context(), f.f.scope(t, f.f.writer, false), result.Receipt.ID)
	if err != nil || !adjudication.Finalized || adjudication.FrozenCounts == nil || *adjudication.FrozenCounts != (reviewexec.FindingCounts{}) {
		t.Fatal("explicit zero finalization missing", err)
	}
	journal, err = f.f.engine.ReadHostedObservations(t.Context(), f.f.project, original.ID)
	if err != nil || len(journal) != 2 || journal[0].Input != baseline || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f, "review_execution_attempts") != 1 || recoveryRuntimeCount(t, f.f, "review_operations") != 1 {
		t.Fatal("replay duplicated native evidence or source", err)
	}
	var completions int
	if err = f.f.store.DB().QueryRow(`SELECT count(*) FROM review_execution_transitions WHERE attempt_id=? AND state='completed'`, original.ID).Scan(&completions); err != nil || completions != 1 {
		t.Fatal("completion duplicated", completions, err)
	}
	afterBody, _ := json.Marshal(record)
	if !bytes.Equal(afterBody, originalBody) || !bytes.Equal(baselineRaw, hostedBridgeBytes(t, f, baselineRef)) {
		t.Fatal("recovery mutated source or original baseline bytes")
	}
}

func TestRetainHostedPlainFinishedAckWithoutInventoryUncredited(t *testing.T) {
	f, record := runtimeStandingFixture(t, nil, nil)
	ack := record.Snapshot.IssueComments[2]
	ack.Body = "<!-- coderabbit-ai-generated:v1 -->\n\n" + strings.Replace(strings.Replace(ack.Body, "✅ Action performed", "Action performed", 1), "Full review finished.", "<dl>\n<dd>\n\nFull review finished.\n\n</dd>\n</dl>", 1)
	record.Snapshot.IssueComments = append(record.Snapshot.IssueComments[:1], ack)
	before := cliAttemptState(t, f.f.store, f.a.ID)
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || result.Complete || result.Reason != "hosted_finished_inventory_unproven" || result.Receipt.ID != "" || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 1 || cliAttemptState(t, f.f.store, f.a.ID) != before {
		t.Fatal("plain acknowledgment invented inventory/credit or lost reservation", result, err)
	}
	retained, err := f.captures.Read(t.Context(), f.a.ID, result.Capture)
	if err != nil || !reflect.DeepEqual(retained, record) {
		t.Fatal("uncertain raw source changed", err)
	}
}
