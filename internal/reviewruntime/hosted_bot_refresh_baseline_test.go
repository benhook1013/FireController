package reviewruntime

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRetainHostedBaselineBotRefreshNativeOnceOnly(t *testing.T) {
	for _, mode := range []string{"identifiers", "unchanged", "prose", "command", "removed_command", "human", "identity", "after_ack", "positive_five", "grouped_six", "new_bot_report", "new_bot_positive", "missing_baseline"} {
		t.Run(mode, func(t *testing.T) {
			var old reviewprovider.IssueComment
			f, record := runtimeStandingFixture(t, func(b *HostedCaptureRecord) {
				old = reviewprovider.IssueComment{ID: 9001, Author: reviewprovider.Author{ID: 9002, Login: "synthetic-status[bot]", Kind: "Bot"}, CreatedAt: b.StartedAt.Add(-time.Hour), UpdatedAt: b.StartedAt.Add(-time.Minute), Body: "Status head=" + strings.Repeat("a", 40) + " run=123.\nReady."}
				if mode == "removed_command" {
					old.Body += "<!-- coderabbit -->"
				}
				if mode == "human" {
					old.Author.Kind = "User"
				}
				if mode != "missing_baseline" && mode != "new_bot_report" && mode != "new_bot_positive" {
					b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, old)
				}
			}, nil)
			// Real API trigger timestamps may follow admission. Keep the admitted
			// boundary unchanged while placing the trigger after immutable baseline.
			for i := range record.Snapshot.IssueComments {
				c := &record.Snapshot.IssueComments[i]
				c.UpdatedAt = c.UpdatedAt.Add(2 * time.Second)
				if i != 1 {
					c.CreatedAt = c.CreatedAt.Add(2 * time.Second)
				}
			}
			record.Snapshot.ObservedAt = record.Snapshot.ObservedAt.Add(2 * time.Second)
			record.FinishedAt = record.Snapshot.ObservedAt
			current := old
			current.UpdatedAt = record.Snapshot.IssueComments[0].CreatedAt.Add(time.Second)
			current.Body = "Status head=" + strings.Repeat("b", 40) + " run=456.\nReady."
			switch mode {
			case "unchanged":
				current.Body = old.Body
			case "prose":
				current.Body += " Changed."
			case "command":
				current.Body += "<!-- coderabbit -->"
			case "removed_command":
				current.Body = "An unrelated current report."
			case "identity":
				current.Author.ID++
			case "new_bot_report", "new_bot_positive":
				current.CreatedAt = record.Snapshot.IssueComments[0].CreatedAt.Add(time.Millisecond)
				current.UpdatedAt = record.Snapshot.ObservedAt
			case "after_ack":
				current.UpdatedAt = record.Snapshot.ObservedAt
			}
			record.Snapshot.IssueComments = append(record.Snapshot.IssueComments, current)
			if mode == "positive_five" || mode == "grouped_six" || mode == "new_bot_positive" {
				trigger := record.Snapshot.IssueComments[0].CreatedAt
				record.Snapshot.IssueComments = append(record.Snapshot.IssueComments[:1], record.Snapshot.IssueComments[2:]...)
				record.Snapshot.IssueComments[len(record.Snapshot.IssueComments)-1].UpdatedAt = record.Snapshot.ObservedAt
				record.Snapshot.Reviews = []reviewprovider.HostedReview{{ID: 200, Author: record.ProviderActor, State: "commented", HeadSHA: record.Intent.HeadSHA, SubmittedAt: trigger.Add(time.Second), Body: "**Actionable comments posted: 5**\n\n" + hostedRetainScope(f.f.baseSHA, f.f.headSHA) + "\n\n" + hostedRetainPublicationMarker()}}
				line := int64(7)
				for i := 1; i <= 5; i++ {
					record.Snapshot.ReviewComments = append(record.Snapshot.ReviewComments, reviewprovider.ReviewComment{ID: int64(300 + i), ReviewID: 200, Author: record.ProviderActor, Body: hostedRetainInline(i, "Preserve exact evidence."), Path: "source.go", Line: &line, HeadSHA: record.Intent.HeadSHA, OriginalHeadSHA: record.Intent.HeadSHA, CreatedAt: trigger.Add(time.Second), UpdatedAt: trigger.Add(time.Second)})
				}
			}
			if mode == "grouped_six" {
				record.Snapshot.Reviews[0].Body = strings.Replace(record.Snapshot.Reviews[0].Body, "Actionable comments posted: 5", "Actionable comments posted: 6", 1)
				record.Snapshot.ReviewComments[0].Body += "\n\n---\n\n" + hostedRetainInline(6, "Retain a second independent finding.")
			}
			before := cliAttemptState(t, f.f.store, f.a.ID)
			result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
			if err != nil {
				t.Fatal(err)
			}
			want := mode == "identifiers" || mode == "unchanged" || mode == "prose" || mode == "after_ack" || mode == "positive_five" || mode == "grouped_six" || mode == "new_bot_positive" || mode == "new_bot_report"
			if result.Complete != want {
				t.Fatalf("complete=%v reason=%s want=%v", result.Complete, result.Reason, want)
			}
			loaded, err := f.captures.Read(t.Context(), f.a.ID, result.Capture)
			if err != nil || !reflect.DeepEqual(loaded, record) || cliAttemptState(t, f.f.store, f.a.ID) != before {
				t.Fatal("source or attempt changed", err)
			}
			if !want {
				if cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
					t.Fatal("held source credited")
				}
				return
			}
			replay, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
			if err != nil || replay != result || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 1 {
				t.Fatal("native evidence not once-only", err)
			}
			if mode == "positive_five" || mode == "grouped_six" || mode == "new_bot_positive" {
				evidence, readErr := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), result.Receipt.ID)
				want := 5
				if mode == "grouped_six" {
					want = 6
				}
				if readErr != nil || evidence.Capture.Review.ReportedFindings != int64(want) || len(evidence.Capture.Review.Findings) != want || len(evidence.Capture.Hosted.Sources) != want {
					t.Fatal("native positive inventory lost", readErr)
				}
				return
			}
			f.a, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "completed", "synthetic_complete", f.a.ExternalID)
			if err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			receipt, err := f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "bot-refresh-finalize", result.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero})
			if err != nil {
				t.Fatal(err)
			}
			again, err := f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "bot-refresh-finalize", result.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero})
			if err != nil || !reflect.DeepEqual(receipt, again) {
				t.Fatal("finalization replay changed", err)
			}
		})
	}
}
