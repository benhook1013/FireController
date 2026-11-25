package reviewprovider

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func continuationFixture(t *testing.T) hfFixture {
	t.Helper()
	roots := make([]string, 35)
	for i := range roots {
		roots[i] = hfInline(i + 1)
	}
	outside := make([]string, 8)
	for i := range outside {
		outside[i] = hfOutside(100+i, "engine/queue.go", "12", fmt.Sprintf("Retain item %d.", i))
	}
	f := hfNew(t, roots, hfOutsideCategory(8, outside...))
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
	second := f.snapshot.Reviews[0]
	second.ID = 402
	second.SubmittedAt = second.SubmittedAt.Add(time.Second)
	second.Body = "<!-- coderabbit-ai-generated:v1 -->\n\n**Review continued from previous batch...**\n\n<!-- coderabbit-review-publication v1 publication=11111111-1111-1111-1111-111111111111 attempt=22222222-2222-2222-2222-222222222222 batch=2/2 -->"
	f.snapshot.Reviews = append(f.snapshot.Reviews, second)
	for i := 18; i < len(f.snapshot.ReviewComments); i++ {
		f.snapshot.ReviewComments[i].ReviewID = second.ID
	}
	return f
}

func TestHostedContinuationPreservesCompleteFindingInventory(t *testing.T) {
	f := continuationFixture(t)
	before := append([]HostedReview(nil), f.snapshot.Reviews...)
	got := hfWant(t, f, true, 43)
	inline, outside := 0, 0
	for _, finding := range got.Findings {
		switch finding.Kind {
		case "inline":
			inline++
		case "outside_diff":
			outside++
		}
	}
	if inline != 35 || outside != 8 || !reflect.DeepEqual(before, f.snapshot.Reviews) || len(got.Publication.Batches) != 2 {
		t.Fatalf("inventory changed: inline=%d outside=%d batches=%d", inline, outside, len(got.Publication.Batches))
	}
}

func TestHostedContinuationJoinsOutOfOrderBatchesByIdentity(t *testing.T) {
	f := continuationFixture(t)
	item := strings.Replace(minorTestItem(108), "Prompt for AI Agents", "Potential next steps", 1)
	f.snapshot.Reviews[1].Body += "\n\n" + minorTestCategory(1, minorTestGroup("engine/queue.go", 1, item))
	f.snapshot.Reviews[0], f.snapshot.Reviews[1] = f.snapshot.Reviews[1], f.snapshot.Reviews[0]

	got := hfWant(t, f, true, 44)
	if len(got.Publication.Batches) != 2 || got.Publication.Batches[0].ID != 401 || got.Publication.Batches[1].ID != 402 {
		t.Fatalf("batch identity changed: %+v", got.Publication.Batches)
	}
	minor := got.Findings[len(got.Findings)-1]
	if minor.Kind != "minor" || minor.ReviewID != 402 || !strings.Contains(minor.Detail, "Potential next steps") {
		t.Fatalf("lost original continuation finding: %+v", minor)
	}
}

func TestHostedContinuationFinishedAcknowledgementCannotReplaceMissingBatch(t *testing.T) {
	f := continuationFixture(t)
	f.snapshot.Reviews = f.snapshot.Reviews[:1]
	acknowledgement := submittedTestFixture(t).snapshot.IssueComments[1]
	acknowledgement.Author = f.bot
	acknowledgement.CreatedAt = f.snapshot.IssueComments[0].CreatedAt.Add(time.Second)
	acknowledgement.UpdatedAt = f.snapshot.ObservedAt
	f.snapshot.IssueComments = append(f.snapshot.IssueComments, acknowledgement)
	got := f.extract(t)
	if !got.Publication.CommandFinished || got.Complete || got.Reason != "missing_publication_batches" || len(got.Findings) != 0 {
		t.Fatalf("missing finding coverage was not kept separate from command completion: finished=%v complete=%v reason=%q", got.Publication.CommandFinished, got.Complete, got.Reason)
	}
}

func TestHostedContinuationCannotSupplyMissingInventory(t *testing.T) {
	for _, mode := range []string{"missing-first", "missing-second", "root-count", "outside-count", "first-batch-heading", "repeated-heading", "unknown-prose", "wrong-ordinal", "wrong-publication", "quoted-heading"} {
		t.Run(mode, func(t *testing.T) {
			f := continuationFixture(t)
			switch mode {
			case "missing-first":
				f.snapshot.Reviews = f.snapshot.Reviews[1:]
			case "missing-second":
				f.snapshot.Reviews = f.snapshot.Reviews[:1]
			case "root-count":
				f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "posted: 35", "posted: 34", 1)
			case "outside-count":
				f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "range comments (8)", "range comments (9)", 1)
			case "first-batch-heading":
				f.snapshot.Reviews[0].Body = "**Review continued from previous batch...**\n" + f.snapshot.Reviews[0].Body
			case "repeated-heading":
				f.snapshot.Reviews[1].Body = strings.Replace(f.snapshot.Reviews[1].Body, "**Review continued from previous batch...**", "**Review continued from previous batch...**\n**Review continued from previous batch...**", 1)
			case "unknown-prose":
				f.snapshot.Reviews[1].Body = strings.Replace(f.snapshot.Reviews[1].Body, "previous batch...", "previous batch... Ignored finding", 1)
			case "wrong-ordinal":
				f.snapshot.Reviews[1].Body = strings.Replace(f.snapshot.Reviews[1].Body, "batch=2/2", "batch=1/2", 1)
			case "wrong-publication":
				f.snapshot.Reviews[1].Body = strings.Replace(f.snapshot.Reviews[1].Body, "publication=11111111", "publication=33333333", 1)
			case "quoted-heading":
				f.snapshot.Reviews[1].Body = strings.Replace(f.snapshot.Reviews[1].Body, "**Review continued", "> **Review continued", 1)
			}
			got := f.extract(t)
			if got.Complete {
				t.Fatalf("%s unexpectedly complete", mode)
			}
		})
	}
}
