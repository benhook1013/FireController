package reviewruntime

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestHostedDefinitionListScopeNativeRetentionRecoveryOnceOnly(t *testing.T) {
	var oldBot reviewprovider.IssueComment
	f, record := runtimeStandingFixture(t, func(b *HostedCaptureRecord) {
		oldBot = reviewprovider.IssueComment{ID: 9001, Author: reviewprovider.Author{ID: 9002, Login: "synthetic-status-bot", Kind: "Bot"}, Body: "Earlier build report.", CreatedAt: b.StartedAt.Add(-time.Hour), UpdatedAt: b.StartedAt.Add(-time.Minute)}
		b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, oldBot)
	}, nil)
	if err := reviews.EnsureSchema(t.Context(), f.f.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.f.engine, err = reviewexec.New(f.f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	recoveryRuntimeMetadata(t, f.f, f.a)
	originalAttempt := f.a
	trigger := record.Snapshot.IssueComments[0]
	trigger.CreatedAt = trigger.CreatedAt.Add(time.Second)
	trigger.UpdatedAt = trigger.CreatedAt
	at := trigger.CreatedAt
	currentBot := oldBot
	currentBot.Body = "Refreshed build report."
	currentBot.UpdatedAt = at.Add(2 * time.Second)
	record.Snapshot.IssueComments = []reviewprovider.IssueComment{trigger, currentBot}
	wrappedScope := "<details>\n<summary>ℹ️ Review info</summary>\n<dl>\n<dd>\n<details>\n<summary>📥 Commits</summary>\n<dl>\n<dd>\nReviewing files that changed from the base of the PR and between " + f.f.baseSHA + " and " + f.f.headSHA + ".\n</dd>\n</dl>\n</details>\n</dd>\n</dl>\n</details>"
	record.Snapshot.Reviews = []reviewprovider.HostedReview{
		{ID: 700, Author: record.ProviderActor, State: "commented", HeadSHA: f.f.baseSHA, SubmittedAt: at.Add(-time.Hour), Body: "Earlier immutable provider review."},
		{ID: 200, Author: record.ProviderActor, State: "commented", HeadSHA: f.f.headSHA, SubmittedAt: at.Add(time.Second), Body: "**Actionable comments posted: 3**\n\n" + wrappedScope + "\n\n" + hostedRetainPublicationMarker()},
	}
	line := int64(7)
	record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{{ID: 600, ReviewID: 700, Author: record.ProviderActor, Path: "older.go", Line: &line, HeadSHA: f.f.baseSHA, OriginalHeadSHA: f.f.baseSHA, CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(time.Second), Body: "Earlier finding with an unrelated later annotation."}}
	for i := 0; i < 3; i++ {
		updated := at.Add(time.Second)
		if i > 0 {
			updated = updated.Add(time.Second)
		}
		record.Snapshot.ReviewComments = append(record.Snapshot.ReviewComments, reviewprovider.ReviewComment{ID: int64(301 + i), ReviewID: 200, Author: record.ProviderActor, Path: "source.go", Line: &line, HeadSHA: f.f.headSHA, OriginalHeadSHA: f.f.headSHA, CreatedAt: at.Add(time.Second), UpdatedAt: updated, Body: hostedRetainInline(i+1, "Keep exact source identity.")})
	}
	record.StartedAt = at
	record.Snapshot.ObservedAt = at.Add(3 * time.Second)
	record.FinishedAt = record.Snapshot.ObservedAt
	before := cliAttemptState(t, f.f.store, f.a.ID)
	retained, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || !retained.Complete || retained.Reason != "capture_recorded" {
		t.Fatal("observed metadata nesting did not retain exact three-finding inventory", retained, err)
	}
	if cliAttemptState(t, f.f.store, f.a.ID) != before {
		t.Fatal("retention changed original attempt")
	}
	raw := hostedBridgeBytes(t, f, retained.Capture)
	loaded, err := f.captures.Read(t.Context(), f.a.ID, retained.Capture)
	if err != nil || !reflect.DeepEqual(loaded, record) {
		t.Fatal("retention changed raw provider facts", err)
	}
	evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), retained.Receipt.ID)
	if err != nil || evidence.AttemptID != f.a.ID || evidence.OperationID != f.a.OperationID || evidence.Anchor != *f.a.Anchor || evidence.Capture.Review.ReportedFindings != 3 || len(evidence.Capture.Review.Findings) != 3 || evidence.Capture.Hosted == nil || !reflect.DeepEqual(evidence.Capture.Hosted.ReviewIDs, []int64{200}) || len(evidence.Capture.Hosted.Sources) != 3 || evidence.Capture.Hosted.ObservedBaseSHA != f.a.Anchor.ParentSHA {
		t.Fatal("native evidence lost source binding", err)
	}
	for i, source := range evidence.Capture.Hosted.Sources {
		if source.ReviewID != 200 || source.CommentID != int64(301+i) || evidence.Capture.Review.Findings[i].Detail != record.Snapshot.ReviewComments[i+1].Body {
			t.Fatal("older review annotation leaked into current inventory")
		}
	}
	again, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || again != retained {
		t.Fatal("retained final-slot replay changed", again, err)
	}
	reader := &hostedRecoveryReader{record: record, repositoryID: record.RepositoryID}
	completed, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
	if err != nil || completed.State != "completed" || completed.EvidenceID != retained.Receipt.ID || len(reader.calls) != 0 {
		t.Fatal("retained evidence did not recover same attempt offline", completed, err)
	}
	bound, err := f.f.engine.ReadHostedRecovery(t.Context(), f.f.project, f.a.ID, "runtime-capability")
	if err != nil || bound.Attempt.ID != originalAttempt.ID || bound.Attempt.OperationID != originalAttempt.OperationID || bound.Attempt.ExternalID != originalAttempt.ExternalID || *bound.Attempt.Anchor != *originalAttempt.Anchor {
		t.Fatal("recovery changed original identity", err)
	}
	transitions := recoveryRuntimeCount(t, f.f, "review_execution_transitions")
	replay, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
	if err != nil || replay.Reason != "already_completed" || replay.EvidenceID != retained.Receipt.ID || len(reader.calls) != 0 || recoveryRuntimeCount(t, f.f, "review_execution_transitions") != transitions {
		t.Fatal("completed replay restarted observation or settlement", replay, err)
	}
	if recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 || cliCount(t, f.f.store, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != 1 || !bytes.Equal(raw, hostedBridgeBytes(t, f, retained.Capture)) {
		t.Fatal("recovery duplicated final evidence/credit or changed raw capture")
	}
	journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
	if err != nil || len(journal) != 2 || journal[0].Input.Kind != "allowance_query" {
		t.Fatal("recovery replaced baseline or appended unnecessary captures", err)
	}
	// The original future-safe fixture must retain the exact full-review command.
	if !strings.HasPrefix(bound.Attempt.Intent, "@coderabbitai full review\n") {
		t.Fatal("recovery changed review command intent")
	}
}
