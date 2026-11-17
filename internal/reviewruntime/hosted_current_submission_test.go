package reviewruntime

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func currentSubmittedReviewRecord(record HostedCaptureRecord, base string) HostedCaptureRecord {
	record = submittedReviewRecord(record, base)
	body := &record.Snapshot.Reviews[0].Body
	const icon = `<picture><img src="https://example.test/prompt.svg" width="16" height="16" align="absmiddle" alt=""></picture>`
	prompt := "<details>\n<summary>" + icon + " Prompt to fix all issues</summary>\n<dl>\n<dd>\n```\nExample text cannot supply review metadata.\n```\n</dd>\n</dl>\n</details>\n"
	*body = strings.Replace(*body, "---\n<details>", "---\n"+prompt+"<h6>\n<details>", 1)
	ignored := "<details>\n<summary>⛔ Files ignored due to path filters: 1</summary>\n<dl>\n<dd>\n<br>\n* `build/generated.js` is excluded by `!build/**`\n</dd>\n</dl>\n</details>\n"
	*body = strings.Replace(*body, "<details>\n<summary>📒 Files selected", ignored+"<details>\n<summary>📒 Files selected", 1)
	*body = strings.Replace(*body, "<!-- This is an auto-generated comment by CodeRabbit for review status -->", "</h6>\n<!-- This is an auto-generated comment by CodeRabbit for review status -->", 1)
	return record
}

func TestRecoverHostedCurrentMarkedEmptyRequiresTerminalEvidence(t *testing.T) {
	for _, mode := range []string{"complete", "missing acknowledgment", "orphan root", "reused acknowledgment"} {
		t.Run(mode, func(t *testing.T) {
			f, captures, attempt, reader := recoveryRuntimeFixture(t, "active")
			recoveryRuntimeMetadata(t, f, attempt)
			record := currentSubmittedReviewRecord(hostedRetainClone(t, reader.record), f.baseSHA)
			review := &record.Snapshot.Reviews[0]
			review.Body = strings.Replace(review.Body, "**Actionable comments posted: 1**", "", 1) + "\n\n" + hostedRetainPublicationMarker()
			ack := &record.Snapshot.IssueComments[len(record.Snapshot.IssueComments)-1]
			ack.CreatedAt = review.SubmittedAt.Add(-time.Second)
			if mode != "orphan root" {
				record.Snapshot.ReviewComments = record.Snapshot.ReviewComments[:0]
			}
			switch mode {
			case "orphan root":
				record.Snapshot.ReviewComments[0].ReviewID++
			case "missing acknowledgment":
				record.Snapshot.IssueComments = record.Snapshot.IssueComments[:len(record.Snapshot.IssueComments)-1]
			case "reused acknowledgment":
				old := *ack
				old.ID++
				old.CreatedAt = old.CreatedAt.Add(-time.Hour)
				old.UpdatedAt = old.UpdatedAt.Add(-time.Hour)
				record.Snapshot.IssueComments = append(record.Snapshot.IssueComments, old)
			}
			reader.record = record
			got, err := recoverRuntime(t, f, captures, attempt, reader)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "complete" {
				if got.State == "completed" || got.EvidenceID != "" || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 {
					t.Fatal("omitted count without terminal proof granted native completion", got)
				}
				return
			}
			if got.State != "completed" || got.EvidenceID == "" || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
				t.Fatal("proven marked empty publication did not recover", got)
			}
			evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), got.EvidenceID)
			if err != nil {
				t.Fatal(err)
			}
			hosted := evidence.Capture.Hosted
			if evidence.AttemptID != attempt.ID || hosted == nil || hosted.SubmittedReview != nil || hosted.StandingSummary != nil || hosted.PublicationID == "" || hosted.ProviderAttemptID == "" || !reflect.DeepEqual(hosted.ReviewIDs, []int64{200}) || hosted.ObservedBaseSHA != f.baseSHA || len(evidence.Capture.Review.Findings) != 0 {
				t.Fatal("marked empty recovery changed source variant or zero inventory")
			}
			calls := len(reader.calls)
			replay, err := recoverRuntime(t, f, captures, attempt, reader)
			if err != nil || replay.Reason != "already_completed" || replay.EvidenceID != got.EvidenceID || len(reader.calls) != calls || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 {
				t.Fatal("marked empty replay repeated collection or credit", replay, err)
			}
		})
	}
}

func TestRecoverHostedCurrentSubmissionRetainsNativeSourceAndFailsClosed(t *testing.T) {
	for _, mode := range []string{"complete", "ignored count mismatch", "ignored selected overlap", "unclosed metadata heading"} {
		t.Run(mode, func(t *testing.T) {
			f, captures, attempt, reader := recoveryRuntimeFixture(t, "active")
			recoveryRuntimeMetadata(t, f, attempt)
			record := currentSubmittedReviewRecord(hostedRetainClone(t, reader.record), f.baseSHA)
			switch mode {
			case "ignored count mismatch":
				record.Snapshot.Reviews[0].Body = strings.Replace(record.Snapshot.Reviews[0].Body, "path filters: 1", "path filters: 2", 1)
			case "ignored selected overlap":
				record.Snapshot.Reviews[0].Body = strings.Replace(record.Snapshot.Reviews[0].Body, "build/generated.js", "source.go", 1)
			case "unclosed metadata heading":
				record.Snapshot.Reviews[0].Body = strings.Replace(record.Snapshot.Reviews[0].Body, "</h6>", "", 1)
			}
			reader.record = record
			got, err := recoverRuntime(t, f, captures, attempt, reader)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "complete" {
				if got.State == "completed" || got.EvidenceID != "" || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 {
					t.Fatal("unsupported current envelope granted native completion", got)
				}
				return
			}
			if got.State != "completed" || got.EvidenceID == "" || got.TriggerCommentID != 100 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
				t.Fatal("current submitted envelope failed same-attempt recovery", got)
			}
			evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), got.EvidenceID)
			if err != nil {
				t.Fatal(err)
			}
			hosted := evidence.Capture.Hosted
			if evidence.AttemptID != attempt.ID || evidence.OperationID != attempt.OperationID || hosted == nil || hosted.SubmittedReview == nil || hosted.SubmittedReview.ProviderRunID != "33333333-3333-4333-8333-333333333333" || hosted.SubmittedReview.AcknowledgementCommentID != 400 || !reflect.DeepEqual(hosted.ReviewIDs, []int64{200}) || hosted.PublicationID != "" || hosted.ProviderAttemptID != "" || hosted.ObservedBaseSHA != f.baseSHA || len(evidence.Capture.Review.Findings) != 1 {
				t.Fatal("current submitted recovery lost original identity or findings")
			}
			retained, err := captures.Read(t.Context(), attempt.ID, CaptureReference{ID: evidence.Capture.CaptureID, SHA256: evidence.Capture.CaptureSHA256, Bytes: evidence.Capture.CaptureBytes})
			if err != nil || !reflect.DeepEqual(retained.Snapshot.IssueComments, record.Snapshot.IssueComments) || !reflect.DeepEqual(retained.Snapshot.Reviews, record.Snapshot.Reviews) || !reflect.DeepEqual(retained.Snapshot.ReviewComments, record.Snapshot.ReviewComments) {
				t.Fatal("current submitted recovery rewrote source", err)
			}
			calls := len(reader.calls)
			replay, err := recoverRuntime(t, f, captures, attempt, reader)
			if err != nil || replay.EvidenceID != got.EvidenceID || replay.Reason != "already_completed" || len(reader.calls) != calls || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 {
				t.Fatal("current submitted replay repeated work", replay, err)
			}
		})
	}
}
