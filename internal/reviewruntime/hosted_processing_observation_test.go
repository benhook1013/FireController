package reviewruntime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedProcessingIsRetainedWhileActiveWithoutFinalCredit(t *testing.T) {
	var old reviewprovider.IssueComment
	f, record := runtimeStandingFixture(t, func(b *HostedCaptureRecord) {
		old = reviewprovider.IssueComment{ID: 501, Body: "Previous synthetic standing summary.", Author: b.ProviderActor, CreatedAt: b.StartedAt.Add(-time.Hour), UpdatedAt: b.StartedAt.Add(-time.Minute)}
		b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, old)
	}, nil)
	body := runtimeStandingBody(f.f.baseSHA, f.f.headSHA)
	body = body[strings.Index(body, "<details>"):strings.Index(body, "<!-- recent_review_end -->")]
	body = strings.Replace(body, "ℹ️ Recent review info", "ℹ️ Review info", 1)
	record.Snapshot.IssueComments[1].Body = "<!-- This is an auto-generated comment: review in progress by coderabbit.ai -->\n> [!NOTE]\n>\n> Currently processing new changes in this PR. This may take a few minutes, please wait...\n" + body + "<!-- end of auto-generated comment: review in progress by coderabbit.ai -->"
	record.Snapshot.IssueComments[1].CreatedAt = old.CreatedAt
	record.Snapshot.IssueComments = record.Snapshot.IssueComments[:2]
	baseline, reason, err := loadHostedStandingBaseline(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || reason != "" {
		t.Fatal("baseline", reason, err)
	}
	parsed, err := reviewprovider.ExtractHostedProcessing(record.Snapshot, baseline.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
	if err != nil || parsed == nil {
		t.Fatalf("synthetic processing did not parse: %v; baseline %s trigger %s", err, baseline.Snapshot.ObservedAt, record.Snapshot.IssueComments[0].CreatedAt)
	}
	before := cliAttemptState(t, f.f.store, f.a.ID)
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || result.Complete {
		t.Fatal("processing changed final evidence", result, err)
	}
	var raw string
	if err := f.f.store.DB().QueryRow(`SELECT body_json FROM review_activity_evidence WHERE attempt_id=?`, f.a.ID).Scan(&raw); err != nil {
		t.Fatal("processing observation missing during active attempt", err)
	}
	var evidence reviewexec.ActivityEvidence
	if json.Unmarshal([]byte(raw), &evidence) != nil || evidence.Kind != "hosted_processing_observed" || evidence.SourceCommentID != 501 || evidence.ProviderRunID != runtimeStandingRun || evidence.TriggerID != "100" || evidence.ObservedAt != record.Snapshot.ObservedAt.Format(time.RFC3339Nano) || evidence.CommentUpdatedAt != record.Snapshot.IssueComments[1].UpdatedAt.Format(time.RFC3339Nano) || evidence.ProcessStartedAt != "" {
		t.Fatal("invalid processing evidence", raw)
	}
	if _, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.f.store.DB().QueryRow(`SELECT count(*) FROM review_activity_evidence WHERE attempt_id=?`, f.a.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("repeated poll created duplicate", count, err)
	}
	if before != cliAttemptState(t, f.f.store, f.a.ID) {
		t.Fatal("diagnostic changed scheduler attempt")
	}
}
