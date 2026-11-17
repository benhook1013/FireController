package reviewruntime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func submittedReviewRecord(record HostedCaptureRecord, base string) HostedCaptureRecord {
	review := &record.Snapshot.Reviews[0]
	review.Body = "<!-- coderabbit-ai-generated:v1 -->\n\n**Actionable comments posted: 1**\n\n---\n<details>\n<summary>ℹ️ Review info</summary>\n<details>\n<summary>⚙️ Run configuration</summary>\n- **Configuration used**: Repository: synthetic/repository/.coderabbit.yaml\n- **Review profile**: ASSERTIVE\n- **Plan**: Advanced\n- **Run ID**: `33333333-3333-4333-8333-333333333333`\n</details>\n<details>\n<summary>📥 Commits</summary>\nReviewing files that changed from the base of the PR and between " + base + " and " + record.Intent.HeadSHA + ".\n</details>\n<details>\n<summary>📒 Files selected for processing: 1</summary>\n* `source.go`\n</details>\n</details>\n<!-- This is an auto-generated comment by CodeRabbit for review status -->"
	ackAt := review.SubmittedAt.Add(time.Second)
	record.Snapshot.IssueComments = append(record.Snapshot.IssueComments, reviewprovider.IssueComment{ID: 400, Author: record.ProviderActor, CreatedAt: ackAt, UpdatedAt: ackAt, Body: "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("a", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\nFull review finished.\n</details>"})
	return record
}

func TestRetainHostedSubmittedReviewPreservesRealIdentityAndSource(t *testing.T) {
	f, captures, attempt, record := hostedRetainFixture(t, false)
	record = submittedReviewRecord(record, f.baseSHA)
	original, _ := json.Marshal(record)
	result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
	if err != nil || !result.Complete || result.Receipt.ID == "" {
		t.Fatalf("submitted positive inventory not retained: %+v %v", result, err)
	}
	retained, err := captures.Read(t.Context(), attempt.ID, result.Capture)
	raw, _ := json.Marshal(retained)
	if err != nil || string(raw) != string(original) {
		t.Fatal("submitted source was rewritten", err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), result.Receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := evidence.Capture.Hosted
	if h == nil || h.SubmittedReview == nil || h.SubmittedReview.ProviderRunID != "33333333-3333-4333-8333-333333333333" || h.SubmittedReview.AcknowledgementCommentID != 400 || !reflect.DeepEqual(h.ReviewIDs, []int64{200}) || h.PublicationID != "" || h.ProviderAttemptID != "" || h.StandingSummary != nil || h.ObservedBaseSHA != f.baseSHA || len(evidence.Capture.Review.Findings) != 1 || evidence.Capture.Review.Findings[0].Title != "Preserve exact evidence." {
		t.Fatal("submitted evidence lost real identities, comparison or finding")
	}
	replay, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
	if err != nil || replay != result {
		t.Fatal("submitted retained replay changed capture", err)
	}
}

func TestRecoverHostedSubmittedReviewCompletesOriginalUncertainAttempt(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "active")
	recoveryRuntimeMetadata(t, f, a)
	complete := submittedReviewRecord(hostedRetainClone(t, reader.record), f.baseSHA)
	unsupportedFinishedSnapshot(&reader.record.Snapshot, reader.record.ProviderActor, 100)
	first, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || first.Reason != "hosted_finished_inventory_unproven" || first.EvidenceID != "" {
		t.Fatal("unsupported original inventory was credited", first, err)
	}
	refs, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || len(refs) != 1 {
		t.Fatal("original unsupported capture lost", err)
	}
	oldRef := refs[0].Input
	reader.record = complete
	final, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || final.State != "completed" || final.EvidenceID == "" || final.TriggerCommentID != 100 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
		t.Fatal("supported original publication did not recover same attempt", final, err)
	}
	if _, err = captures.Read(t.Context(), a.ID, CaptureReference{ID: oldRef.CaptureID, SHA256: oldRef.CaptureSHA256, Bytes: oldRef.CaptureBytes}); err != nil {
		t.Fatal("recovery discarded earlier raw evidence", err)
	}
	calls := len(reader.calls)
	again, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || again.Reason != "already_completed" || again.EvidenceID != final.EvidenceID || len(reader.calls) != calls {
		t.Fatal("completed original was observed again", again, err)
	}
}

func TestRecoverHostedGroupedInlineCompletesOriginalActiveAttempt(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "active")
	recoveryRuntimeMetadata(t, f, a)
	reader.record.Snapshot.Reviews[0].Body = strings.Replace(reader.record.Snapshot.Reviews[0].Body, "Actionable comments posted: 1", "Actionable comments posted: 2", 1)
	reader.record.Snapshot.ReviewComments[0].Body += "\n\n---\n\n" + hostedRetainInline(2, "Preserve the grouped result.")
	original, _ := json.Marshal(reader.record)
	final, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || final.State != "completed" || final.EvidenceID == "" || final.TriggerCommentID != 100 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
		t.Fatal("grouped original attempt did not settle once", final, err)
	}
	after, _ := json.Marshal(reader.record)
	if string(after) != string(original) {
		t.Fatal("recovery changed source")
	}
	calls := len(reader.calls)
	replay, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || replay.Reason != "already_completed" || replay.EvidenceID != final.EvidenceID || len(reader.calls) != calls {
		t.Fatal("grouped completion replay collected or credited again", replay, err)
	}
}
