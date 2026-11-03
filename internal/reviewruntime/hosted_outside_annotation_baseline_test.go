package reviewruntime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRetainHostedOutsideOnlyAnnotationsAndParserDiagnostic(t *testing.T) {
	var report reviewprovider.IssueComment
	f, record := runtimeAnnotatedStandingFixture(t, func(b *HostedCaptureRecord) {
		report = reviewprovider.IssueComment{ID: 9001, Author: reviewprovider.Author{ID: 9002, Login: "synthetic-status[bot]", Kind: "Bot"}, CreatedAt: b.StartedAt.Add(-time.Hour), UpdatedAt: b.StartedAt.Add(-time.Minute), Body: "Build pending."}
		b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, report)
	})
	for i := range record.Snapshot.IssueComments {
		c := &record.Snapshot.IssueComments[i]
		c.UpdatedAt = c.UpdatedAt.Add(2 * time.Second)
		if i != 1 {
			c.CreatedAt = c.CreatedAt.Add(2 * time.Second)
		}
	}
	trigger := record.Snapshot.IssueComments[0].CreatedAt
	record.Snapshot.ReviewComments[0].UpdatedAt = trigger.Add(time.Second)
	record.Snapshot.ObservedAt = record.Snapshot.ObservedAt.Add(2 * time.Second)
	record.FinishedAt = record.Snapshot.ObservedAt
	report.Body = "Build passed."
	report.UpdatedAt = trigger.Add(time.Second)
	record.Snapshot.IssueComments = []reviewprovider.IssueComment{record.Snapshot.IssueComments[0], report}
	outside := "> [!CAUTION]\n> Some comments are outside the diff and are listed below.\n>\n> **⚠️ Outside diff range comments (1)**\n>\n> <details>\n> <summary><strong>🟡 Minor</strong> · Preserve source. · <code>events.go:19</code></summary><blockquote>\n>\n> `engine/events.go:19`\n> **📐 Maintainability & Code Quality** | **🟡 Minor**\n>\n> **Preserve the outside finding.**\n>\n> Keep its original source text.\n>\n> <!-- cr-comment:v1:000000000000000000000002 -->\n>\n> </blockquote></details>\n\n"
	current := reviewprovider.HostedReview{ID: 901, Author: record.ProviderActor, State: "commented", HeadSHA: record.Intent.HeadSHA, SubmittedAt: trigger.Add(2 * time.Second), Body: outside + hostedRetainScope(f.f.baseSHA, f.f.headSHA) + "\n\n" + hostedRetainPublicationMarker()}
	record.Snapshot.Reviews = append(record.Snapshot.Reviews, current)
	before, _ := json.Marshal(record)
	state := cliAttemptState(t, f.f.store, f.a.ID)
	bad := record
	bad.Snapshot.Reviews = append([]reviewprovider.HostedReview(nil), record.Snapshot.Reviews...)
	bad.Snapshot.Reviews[1].Body = strings.Replace(current.Body, "comments (1)", "comments (2)", 1)
	blocked, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, bad)
	if err != nil || blocked.Complete || blocked.Reason != "invalid_actionable_count" || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliAttemptState(t, f.f.store, f.a.ID) != state {
		t.Fatal("qualified parser failure hidden or credited", blocked, err)
	}
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || !result.Complete || result.Reason != "capture_recorded" {
		t.Fatal("proven old annotations blocked valid outside result", result, err)
	}
	evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), result.Receipt.ID)
	if err != nil || evidence.AttemptID != f.a.ID || evidence.OperationID != f.a.OperationID || evidence.Capture.Hosted == nil || evidence.Capture.Hosted.TriggerCommentID != 100 || !reflect.DeepEqual(evidence.Capture.Hosted.ReviewIDs, []int64{901}) || evidence.Capture.Review.ReportedFindings != 1 || len(evidence.Capture.Review.Findings) != 1 || len(evidence.Capture.Hosted.Sources) != 1 {
		t.Fatal("original result identity or finding count changed", err)
	}
	finding, source := evidence.Capture.Review.Findings[0], evidence.Capture.Hosted.Sources[0]
	if finding.Severity != "low" || source.Kind != "outside_diff" || source.ReviewID != 901 || source.CommentID != 0 || !strings.Contains(current.Body, finding.Detail) {
		t.Fatal("old root counted or literal outside finding changed")
	}
	replay, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	after, _ := json.Marshal(record)
	if err != nil || replay != result || !reflect.DeepEqual(before, after) || cliAttemptState(t, f.f.store, f.a.ID) != state || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 || cliCount(t, f.f.store, `SELECT count(*) FROM review_round_completions`) != 0 {
		t.Fatal("replay changed raw source/attempt/accounting", err)
	}
}
