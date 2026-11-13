package reviewruntime

import (
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func runtimeSubmittedSettlementFixture(t *testing.T, edit func(*reviewexec.HostedObservationInput)) (*runtimeAllowanceFixture, HostedCaptureRecord) {
	t.Helper()
	f, record := runtimeStandingFixture(t, nil, edit)
	// Whole-second GitHub timestamps still put this trigger strictly after the
	// journaled allowance observation and actual local pre-POST boundary.
	trigger := record.Snapshot.IssueComments[0].CreatedAt.Add(time.Second)
	record.Snapshot.IssueComments = record.Snapshot.IssueComments[:1]
	record.Snapshot.IssueComments[0].CreatedAt = trigger
	record.Snapshot.IssueComments[0].UpdatedAt = trigger
	record.Snapshot.Reviews = []reviewprovider.HostedReview{{ID: 200, Author: record.ProviderActor, State: "commented", HeadSHA: f.f.headSHA, SubmittedAt: trigger.Add(200 * time.Millisecond)}}
	record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{{ID: 300, ReviewID: 200, Author: record.ProviderActor, Body: hostedRetainInline(1, "Preserve the settled publication."), Path: "source.go", HeadSHA: f.f.headSHA, OriginalHeadSHA: f.f.headSHA, CreatedAt: trigger.Add(100 * time.Millisecond), UpdatedAt: trigger.Add(300 * time.Millisecond)}}
	record = submittedReviewRecord(record, f.f.baseSHA)
	ack := &record.Snapshot.IssueComments[1]
	ack.CreatedAt = trigger.Add(100 * time.Millisecond)
	ack.UpdatedAt = trigger.Add(400 * time.Millisecond)
	record.Snapshot.ObservedAt = trigger.Add(500 * time.Millisecond)
	record.FinishedAt = record.Snapshot.ObservedAt
	return f, record
}

func TestRetainHostedSubmittedSettlementOriginalBaselineAndRawReplay(t *testing.T) {
	f, record := runtimeSubmittedSettlementFixture(t, nil)
	strict, err := reviewprovider.ExtractHostedFindings(record.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
	if err != nil || strict.Complete {
		t.Fatal("strict fixture must remain uncredited", strict, err)
	}
	first, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || !first.Complete {
		t.Fatal("original bound settlement failed", first, err)
	}
	stored, err := f.captures.Read(t.Context(), f.a.ID, first.Capture)
	if err != nil || !reflect.DeepEqual(stored, record) {
		t.Fatal("raw observation changed", err)
	}
	evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), first.Receipt.ID)
	if err != nil || evidence.Capture.Hosted == nil || evidence.Capture.Hosted.SubmittedReview == nil || evidence.Capture.Hosted.SubmittedReview.AcknowledgementCommentID != 400 || len(evidence.Capture.Review.Findings) != 1 || evidence.Capture.FinishedAt != record.Snapshot.IssueComments[1].UpdatedAt.Format(time.RFC3339Nano) {
		t.Fatal("settled provenance lost", evidence, err)
	}
	replay, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || replay != first || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 {
		t.Fatal("replay minted evidence or completion", replay, err)
	}
}

func TestRetainHostedSubmittedSettlementRejectsMismatchedBaselineJournal(t *testing.T) {
	f, record := runtimeSubmittedSettlementFixture(t, func(in *reviewexec.HostedObservationInput) {
		at, _ := time.Parse(time.RFC3339Nano, in.StartedAt)
		in.StartedAt = at.Add(-time.Nanosecond).Format(time.RFC3339Nano)
	})
	got, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || got.Complete || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 0 {
		t.Fatal("unbound baseline credited", got, err)
	}
}

func TestRecoverHostedSubmittedSettlementKeepsOriginalAttempt(t *testing.T) {
	f, record := runtimeSubmittedSettlementFixture(t, nil)
	recoveryRuntimeMetadata(t, f.f, f.a)
	retained, _, err := retainHostedObservation(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || retained.Reason != "capture_retained" {
		t.Fatal(retained, err)
	}
	f.a, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "uncertain", "hosted_finished_inventory_unproven", f.a.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	reader := &hostedRecoveryReader{record: record, repositoryID: record.RepositoryID}
	got, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
	if err != nil || got.State != "completed" || got.EvidenceID == "" || got.TriggerCommentID != 100 || len(reader.calls) != 0 {
		t.Fatal("retained original did not settle offline", got, err)
	}
	evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), got.EvidenceID)
	if err != nil || evidence.AttemptID != f.a.ID || evidence.OperationID != f.a.OperationID || evidence.Capture.CaptureID != retained.Capture.ID || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 0 {
		t.Fatal("recovery lost original ownership", evidence, err)
	}
	again, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
	if err != nil || again.Reason != "already_completed" || again.EvidenceID != got.EvidenceID || len(reader.calls) != 0 {
		t.Fatal("recovery repeated provider work", again, err)
	}
}
