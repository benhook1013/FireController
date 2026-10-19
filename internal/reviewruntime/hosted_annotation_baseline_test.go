package reviewruntime

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func runtimeAnnotatedStandingFixture(t *testing.T, edit func(*HostedCaptureRecord)) (*runtimeAllowanceFixture, HostedCaptureRecord) {
	t.Helper()
	var review reviewprovider.HostedReview
	var root reviewprovider.ReviewComment
	f, record := runtimeStandingFixture(t, func(baseline *HostedCaptureRecord) {
		at := baseline.StartedAt.Add(-time.Hour)
		oldHead := strings.Repeat("a", 40)
		line := int64(11)
		review = reviewprovider.HostedReview{ID: 801, Body: "Original synthetic review summary.", State: "commented", HeadSHA: oldHead, Author: baseline.ProviderActor, SubmittedAt: at}
		root = reviewprovider.ReviewComment{ID: 802, ReviewID: review.ID, Body: "_⚠️ Potential issue_ | _🟠 Major_\n\n**Keep synthetic state.**\n\n<!-- cr-comment:v1:000000000000000000000071 -->", Author: baseline.ProviderActor, Path: "engine/prior.go", Line: &line, OriginalLine: &line, Side: "RIGHT", HeadSHA: oldHead, OriginalHeadSHA: oldHead, CreatedAt: at, UpdatedAt: at}
		baseline.Snapshot.Reviews = []reviewprovider.HostedReview{review}
		baseline.Snapshot.ReviewComments = []reviewprovider.ReviewComment{root}
		if edit != nil {
			edit(baseline)
		}
	}, nil)
	record.Snapshot.Reviews = []reviewprovider.HostedReview{review}
	root.Body += "\n\n✅ Addressed in commit " + record.Intent.HeadSHA[:7]
	root.UpdatedAt = record.Snapshot.IssueComments[0].CreatedAt.Add(time.Second)
	record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{root}
	return f, record
}

func TestRetainHostedAnnotatedStandingZeroFinalizationAndReplay(t *testing.T) {
	f, record := runtimeAnnotatedStandingFixture(t, nil)
	strict, err := reviewprovider.ExtractHostedFindings(record.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
	if err != nil || strict.Complete || strict.Publication.State != "active" || strict.Publication.Trigger.State != "adopted" {
		t.Fatal("synthetic old-root annotation must remain strictly incomplete", strict, err)
	}
	journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
	if err != nil || len(journal) != 1 {
		t.Fatal(journal, err)
	}
	input := journal[0].Input
	baselineRef := CaptureReference{ID: input.CaptureID, SHA256: input.CaptureSHA256, Bytes: input.CaptureBytes}
	baseline, err := f.captures.Read(t.Context(), f.a.ID, baselineRef)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(record)
	baselineBefore, _ := json.Marshal(baseline)
	baselineRawBefore := hostedBridgeBytes(t, f, baselineRef)
	currentRef, err := f.captures.Save(t.Context(), f.a.ID, record)
	if err != nil {
		t.Fatal(err)
	}
	currentRawBefore := hostedBridgeBytes(t, f, currentRef)
	state := cliAttemptState(t, f.f.store, f.a.ID)
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || !result.Complete || result.Reason != "capture_recorded" || result.Capture != currentRef {
		t.Fatal("proven annotation prevented final evidence", result, err)
	}
	evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), result.Receipt.ID)
	if err != nil || evidence.Capture.Hosted == nil || evidence.Capture.Hosted.StandingSummary == nil || evidence.Capture.Hosted.StandingSummary.ProviderRunID != runtimeStandingRun || evidence.Capture.Review.ReportedFindings != 0 || len(evidence.Capture.Review.Findings) != 0 || len(evidence.Capture.Hosted.ReviewIDs) != 0 || len(evidence.Capture.Hosted.Sources) != 0 {
		t.Fatal("not exact standing-zero evidence", evidence, err)
	}
	raw, err := f.captures.Read(t.Context(), f.a.ID, result.Capture)
	if err != nil || !reflect.DeepEqual(raw, record) || raw.Snapshot.ReviewComments[0].Body != record.Snapshot.ReviewComments[0].Body {
		t.Fatal("annotated raw body lost", raw, err)
	}
	baselineAfter, err := f.captures.Read(t.Context(), f.a.ID, baselineRef)
	baselineBytes, _ := json.Marshal(baselineAfter)
	currentBytes, _ := json.Marshal(record)
	if err != nil || !bytes.Equal(baselineBytes, baselineBefore) || !bytes.Equal(currentBytes, before) || !bytes.Equal(hostedBridgeBytes(t, f, baselineRef), baselineRawBefore) || !bytes.Equal(hostedBridgeBytes(t, f, currentRef), currentRawBefore) || cliAttemptState(t, f.f.store, f.a.ID) != state {
		t.Fatal("attribution mutated raw inputs or settled attempt", err)
	}
	f.a, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "completed", "synthetic_complete", f.a.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err = f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "annotated-standing-finalize", result.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
		t.Fatal(err)
	}
	replay, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || replay != result || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 1 {
		t.Fatal("exact replay duplicated final evidence", replay, err)
	}
	journal, err = f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
	if err != nil || len(journal) != 2 {
		t.Fatal("exact replay changed baseline/current journal", journal, err)
	}
}

func TestRetainHostedAnnotatedStandingZeroRefusesUnprovenBaseline(t *testing.T) {
	for _, mode := range []string{"missing", "late", "wrong_repository", "query_edited", "reused_run", "body_changed", "path_changed", "review_changed", "annotation_after_finish"} {
		t.Run(mode, func(t *testing.T) {
			f, record := runtimeAnnotatedStandingFixture(t, func(baseline *HostedCaptureRecord) {
				switch mode {
				case "wrong_repository":
					baseline.Repository = "other/repo"
				case "query_edited":
					baseline.Snapshot.IssueComments[0].Body += " edited"
				case "reused_run":
					baseline.Snapshot.IssueComments[1].Body += " earlier " + strings.ToUpper(runtimeStandingRun)
				}
			})
			switch mode {
			case "missing":
				if _, err := f.f.store.DB().Exec(`DELETE FROM review_hosted_observations WHERE attempt_id=?`, f.a.ID); err != nil {
					t.Fatal(err)
				}
			case "late":
				journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
				if err != nil || len(journal) != 1 {
					t.Fatal(journal, err)
				}
				finish, _ := time.Parse(time.RFC3339Nano, journal[0].Input.FinishedAt)
				if _, err = f.f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=2`, finish.Add(-time.Nanosecond).Format(time.RFC3339Nano), f.a.ID); err != nil {
					t.Fatal(err)
				}
			case "body_changed":
				record.Snapshot.ReviewComments[0].Body = strings.Replace(record.Snapshot.ReviewComments[0].Body, "Keep", "Rewrite", 1)
			case "path_changed":
				record.Snapshot.ReviewComments[0].Path = "engine/moved.go"
			case "review_changed":
				record.Snapshot.Reviews[0].Body += " New finding."
			case "annotation_after_finish":
				record.Snapshot.ReviewComments[0].UpdatedAt = record.Snapshot.ObservedAt
			}
			before := cliAttemptState(t, f.f.store, f.a.ID)
			strict, strictErr := reviewprovider.ExtractHostedFindings(record.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
			if strictErr != nil || strict.Complete {
				t.Fatal("negative fixture must be strict incomplete", strict, strictErr)
			}
			result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
			if err != nil || result.Complete || !validHostedReference(result.Capture) || cliAttemptState(t, f.f.store, f.a.ID) != before || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
				t.Fatal("unproven annotation gained evidence or lost raw source", result, err)
			}
			if mode == "reused_run" && result.Reason != "standing_run_not_new" {
				t.Fatal("baseline UUID guard bypassed", result)
			}
			if mode != "reused_run" && result.Reason != strict.Reason {
				t.Fatal("optional attribution changed strict hold reason", result, strict)
			}
			raw, err := f.captures.Read(t.Context(), f.a.ID, result.Capture)
			if err != nil || !reflect.DeepEqual(raw, record) {
				t.Fatal("held original body not retained", err)
			}
		})
	}
}

func TestRetainHostedAnnotatedStandingZeroHeldObservationLeavesFinalSlotFree(t *testing.T) {
	f, valid := runtimeAnnotatedStandingFixture(t, nil)
	changed := valid
	changed.Snapshot.ReviewComments = append([]reviewprovider.ReviewComment(nil), valid.Snapshot.ReviewComments...)
	changed.Snapshot.ReviewComments[0].Body += "\nNew unknown finding."
	held, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, changed)
	if err != nil || held.Complete || !validHostedReference(held.Capture) || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
		t.Fatal("unsupported body occupied final slot", held, err)
	}
	completed, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, valid)
	if err != nil || !completed.Complete || completed.Capture == held.Capture || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 1 {
		t.Fatal("held raw observation prevented later proven evidence", completed, err)
	}
	if _, err = f.captures.Read(t.Context(), f.a.ID, held.Capture); err != nil {
		t.Fatal("earlier unsupported raw body was discarded", err)
	}
}

func TestRetainHostedAnnotatedStandingZeroLatestInvalidBaselineDoesNotFallback(t *testing.T) {
	f, record := runtimeAnnotatedStandingFixture(t, nil)
	journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
	if err != nil || len(journal) != 1 {
		t.Fatal(journal, err)
	}
	in := journal[0].Input
	latest, err := f.captures.Read(t.Context(), f.a.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes})
	if err != nil {
		t.Fatal(err)
	}
	latest.Repository = "other/repo"
	ref, err := f.captures.Save(t.Context(), f.a.ID, latest)
	if err != nil {
		t.Fatal(err)
	}
	in.CaptureID, in.CaptureSHA256, in.CaptureBytes = ref.ID, ref.SHA256, ref.Bytes
	if _, err = f.f.engine.RecordHostedObservation(t.Context(), f.f.project, f.a.ID, in); err != nil {
		t.Fatal(err)
	}
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || result.Complete || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
		t.Fatal("latest invalid source replaced by older valid baseline", result, err)
	}
}
