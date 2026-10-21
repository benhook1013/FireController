package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func externalFixture(t *testing.T) hfFixture {
	f := hfNew(t, []string{hfInline(1)}, "")
	f.snapshot.IssueComments = []IssueComment{}
	if f.snapshot.ReviewComments == nil {
		f.snapshot.ReviewComments = []ReviewComment{}
	}
	return f
}
func externalDiscover(t *testing.T, f hfFixture) []ExternalHostedRun {
	t.Helper()
	runs, err := DiscoverExternalHosted(f.snapshot, f.bot)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}
func TestExternalHostedIndependentBatchesOldHeadAndExactFindings(t *testing.T) {
	f := externalFixture(t)
	one := &f.snapshot.Reviews[0]
	one.Body = "**Actionable comments posted: 1**\n\n" + hfOutsideCategory(1, hfOutside(2, "outside.go", "4", "Outside finding")) + "\n\n" + strings.Replace(publicationTestMarker(1, 2), publicationTestID, "11111111-1111-1111-1111-111111111111", 1)
	two := *one
	two.ID = 402
	two.SubmittedAt = two.SubmittedAt.Add(time.Second)
	two.Body = hfDuplicateCategory(1, hfDuplicateGroup("duplicate.go", 1, hfDuplicateItem(3, "7-7", "Duplicate finding"))) + "\n\n" + strings.Replace(publicationTestMarker(2, 2), publicationTestID, "11111111-1111-1111-1111-111111111111", 1)
	old := *one
	old.ID = 403
	old.HeadSHA = strings.Repeat("b", 40)
	old.SubmittedAt = old.SubmittedAt.Add(-time.Hour)
	old.Body = "**Actionable comments posted: 0**\n" + strings.Replace(publicationTestMarker(1, 1), publicationTestID, "33333333-3333-3333-3333-333333333333", 1)
	f.snapshot.Reviews = []HostedReview{two, old, *one}
	reply := f.snapshot.ReviewComments[0]
	reply.ID = 999
	reply.InReplyToID = f.snapshot.ReviewComments[0].ID
	reply.Body = "Not a new finding"
	f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, reply)
	got := externalDiscover(t, f)
	if len(got) != 2 {
		t.Fatalf("sources=%d", len(got))
	}
	if got[0].SourceID != "publication:33333333-3333-3333-3333-333333333333" || got[0].HeadSHA != old.HeadSHA || !got[0].Inventory.Complete || len(got[0].Inventory.Findings) != 0 {
		t.Fatal("lost independent old-head source")
	}
	run := got[1]
	if run.SourceID != "publication:11111111-1111-1111-1111-111111111111" || run.HeadSHA != f.intent.HeadSHA || !run.FinishedAt.Equal(two.SubmittedAt) || !reflect.DeepEqual(run.ReviewIDs, []int64{401, 402}) || !run.Inventory.Complete || len(run.Inventory.Findings) != 3 {
		t.Fatalf("batch inventory not complete: %s/%v/%d", run.Inventory.Reason, run.Inventory.Complete, len(run.Inventory.Findings))
	}
	if run.Inventory.Findings[0].Kind != "inline" || run.Inventory.Findings[1].Kind != "outside_diff" || run.Inventory.Findings[2].Kind != "duplicate" || run.TriggerCommentID != 0 || run.NativeOperationID != "" || run.Inventory.Publication.Trigger.CommentID != 0 {
		t.Fatal("lost real inventory or fabricated controller attribution")
	}
	if !reflect.DeepEqual(got, externalDiscover(t, f)) {
		t.Fatal("repeat changed source identities or results")
	}
}
func TestExternalHostedSubmittedAndStandingSourcesNeedNoInventedAck(t *testing.T) {
	positive := submittedTestFixture(t)
	positive.snapshot.IssueComments = []IssueComment{}
	got := externalDiscover(t, positive)
	if len(got) != 1 || got[0].SourceID != "run:"+submittedTestRun || got[0].ProviderRunID != submittedTestRun || !got[0].Inventory.Complete || len(got[0].Inventory.Findings) != 2 || len(got[0].ReviewIDs) != 1 || got[0].Inventory.Publication.SubmittedReview != nil {
		t.Fatalf("submitted source lost: sources=%d", len(got))
	}
	s, intent, poster, bot, earliest := standingTestFixture(t)
	s.IssueComments = []IssueComment{s.IssueComments[1]}
	zero := hfFixture{snapshot: s, intent: intent, poster: poster, bot: bot, earliest: earliest}
	got = externalDiscover(t, zero)
	if len(got) != 1 || !got[0].Inventory.Complete || got[0].ProviderRunID == "" || got[0].SourceID != "run:"+got[0].ProviderRunID || len(got[0].ReviewIDs) != 0 || len(got[0].Inventory.Findings) != 0 || got[0].Inventory.Publication.StandingZero != nil || got[0].TriggerCommentID != 0 {
		t.Fatal("standing source invented a review/acknowledgement or lost explicit zero")
	}
	if _, proven := ResolveHostedComparison(got[0].Inventory.Publication, intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("b", 40)); proven {
		t.Fatal("observed standing body conferred native comparison proof")
	}
}
func TestExternalHostedUnsupportedRecognizedInventoryStaysIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*hfFixture)
		reason string
	}{
		{"missing-batch", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
		}, "missing_publication_batches"},
		{"unsupported-summary", func(f *hfFixture) { f.snapshot.Reviews[0].Body += "\nUnclassified substantive summary" }, "summary_format_unsupported"},
		{"malformed-finding", func(f *hfFixture) { f.snapshot.ReviewComments[0].Body = "Unsupported real root" }, "finding_marker_missing"},
		{"wrong-root-head", func(f *hfFixture) { f.snapshot.ReviewComments[0].OriginalHeadSHA = strings.Repeat("d", 40) }, "root_binding_mismatch"},
		{"wrong-root-actor", func(f *hfFixture) { f.snapshot.ReviewComments[0].Author.ID++ }, "root_binding_mismatch"},
		{"wrong-count", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "posted: 1", "posted: 0", 1)
		}, "root_count_mismatch"},
		{"duplicate-batch", func(f *hfFixture) {
			copy := f.snapshot.Reviews[0]
			copy.ID++
			f.snapshot.Reviews = append(f.snapshot.Reviews, copy)
		}, "conflicting_publication_evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := externalFixture(t)
			tc.change(&f)
			got := externalDiscover(t, f)
			if len(got) != 1 || got[0].Inventory.Complete || got[0].Inventory.Reason != tc.reason {
				t.Fatalf("unsupported source became complete/dropped: sources=%d expected=%s", len(got), tc.reason)
			}
		})
	}
	f := submittedTestFixture(t)
	f.snapshot.IssueComments = []IssueComment{}
	f.snapshot.Reviews[0].Body += "\nUnsupported trailing metadata"
	if got := externalDiscover(t, f); len(got) != 1 || got[0].Inventory.Complete {
		t.Fatal("recognized unsupported submitted source discarded or credited")
	}
}
func TestExternalHostedTrustedIdentityVisibilityAndChatter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*hfFixture)
	}{
		{"same-login-other-id", func(f *hfFixture) { f.snapshot.Reviews[0].Author.ID++ }},
		{"same-id-other-kind", func(f *hfFixture) { f.snapshot.Reviews[0].Author.Kind = "User" }},
		{"quoted-marker", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.ReplaceAll(f.snapshot.Reviews[0].Body, "<!-- coderabbit-review-publication", "> <!-- coderabbit-review-publication")
		}},
		{"fenced-marker", func(f *hfFixture) { f.snapshot.Reviews[0].Body = "```\n" + f.snapshot.Reviews[0].Body + "\n```" }},
		{"empty-ack-review", func(f *hfFixture) { f.snapshot.Reviews[0].Body = "" }},
		{"pending", func(f *hfFixture) {
			f.snapshot.Reviews[0].State = "pending"
			f.snapshot.Reviews[0].SubmittedAt = time.Time{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := externalFixture(t)
			tc.change(&f)
			if len(externalDiscover(t, f)) != 0 {
				t.Fatal("unattributed/quoted/chatter became an external run")
			}
		})
	}
	f := externalFixture(t)
	f.snapshot.Reviews[0].Author.Login = "renamed-bot"
	if len(externalDiscover(t, f)) != 1 {
		t.Fatal("login rename changed stable identity")
	}
}
func TestExternalHostedStrictCompleteSnapshotAndCopyIsolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*hfFixture)
	}{
		{"partial", func(f *hfFixture) { f.snapshot.Complete = false }},
		{"unknown-comments", func(f *hfFixture) { f.snapshot.IssueComments = nil }},
		{"unknown-roots", func(f *hfFixture) { f.snapshot.ReviewComments = nil }},
		{"duplicate-id", func(f *hfFixture) { f.snapshot.Reviews = append(f.snapshot.Reviews, f.snapshot.Reviews[0]) }},
		{"bad-observed-time", func(f *hfFixture) { f.snapshot.ObservedAt = time.Time{} }},
		{"run-id-reused-other-head", func(f *hfFixture) {
			copy := f.snapshot.Reviews[0]
			copy.ID++
			copy.HeadSHA = strings.Repeat("d", 40)
			f.snapshot.Reviews = append(f.snapshot.Reviews, copy)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := externalFixture(t)
			tc.change(&f)
			got, err := DiscoverExternalHosted(f.snapshot, f.bot)
			if err == nil || len(got) != 0 {
				t.Fatal("invalid source collection was accepted")
			}
		})
	}
	f := externalFixture(t)
	line := int64(7)
	f.snapshot.ReviewComments[0].Line = &line
	got := externalDiscover(t, f)
	*got[0].Inventory.Publication.Roots[0].Line = 99
	if line != 7 {
		t.Fatal("discovery mutated caller-owned location")
	}
	wrong := f.bot
	wrong.Kind = "User"
	if _, err := DiscoverExternalHosted(f.snapshot, wrong); err == nil {
		t.Fatal("nonbot trusted selector accepted")
	}
}
func TestExternalHostedNativeMarkerIsOnlyVerifiedByConsumer(t *testing.T) {
	f := externalFixture(t)
	body, _ := HostedTriggerBody(f.intent)
	command := IssueComment{ID: 301, Author: f.poster, Body: body, CreatedAt: f.earliest, UpdatedAt: f.earliest}
	f.snapshot.IssueComments = []IssueComment{command}
	got := externalDiscover(t, f)
	if len(got) != 1 || !got[0].Inventory.Complete || got[0].NativeOperationID != f.intent.OperationID || got[0].TriggerCommentID != command.ID || got[0].Inventory.Publication.Trigger.CommentID != 0 {
		t.Fatal("native marker suppressed source or fabricated native publication attribution")
	}
	manual := command
	manual.ID++
	manual.Body = "@coderabbitai full review"
	manual.CreatedAt = command.CreatedAt.Add(time.Millisecond)
	manual.UpdatedAt = manual.CreatedAt
	f.snapshot.IssueComments = append(f.snapshot.IssueComments, manual)
	if got = externalDiscover(t, f); len(got) != 1 || got[0].NativeOperationID != "" || got[0].TriggerCommentID != 0 {
		t.Fatal("manual command did not close native hint window")
	}
	f.snapshot.IssueComments = []IssueComment{command}
	f.snapshot.IssueComments[0].UpdatedAt = f.snapshot.ObservedAt
	if got = externalDiscover(t, f); len(got) != 1 || got[0].NativeOperationID != "" {
		t.Fatal("edited native marker supplied original command identity")
	}
}
