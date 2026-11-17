package reviewprovider

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExternalHostedMalformedVisibleMarkersRetainActualReview(t *testing.T) {
	for _, kind := range []string{"malformed", "duplicate", "conflicting", "invalid-ordinal"} {
		t.Run(kind, func(t *testing.T) {
			f := externalFixture(t)
			review := &f.snapshot.Reviews[0]
			switch kind {
			case "malformed":
				review.Body = strings.Replace(review.Body, "publication=", "publication=broken-", 1)
			case "duplicate":
				review.Body += "\n" + publicationTestMarker(1, 1)
			case "conflicting":
				review.Body += "\n" + strings.Replace(publicationTestMarker(1, 1), publicationTestID, "99999999-9999-4999-8999-999999999999", 1)
			case "invalid-ordinal":
				review.Body = strings.Replace(review.Body, "batch=1/1", "batch=2/1", 1)
			}
			want := *review
			got := externalDiscover(t, f)
			if len(got) != 1 {
				t.Fatalf("recognized malformed provider review disappeared: sources=%d", len(got))
			}
			run := got[0]
			pub := run.Inventory.Publication
			if run.SourceID != "review:"+strconv.FormatInt(want.ID, 10) || run.HeadSHA != want.HeadSHA || !run.FinishedAt.Equal(want.SubmittedAt) || !pub.FinishedAt.Equal(want.SubmittedAt) || !reflect.DeepEqual(run.ReviewIDs, []int64{want.ID}) || !reflect.DeepEqual(pub.EligibleReviews, []HostedReview{want}) {
				t.Fatal("malformed review identity/body/time changed", run)
			}
			if run.Inventory.Complete || run.Inventory.Reason != "conflicting_publication_evidence" || pub.State != "ambiguous" || pub.Reason != run.Inventory.Reason || len(run.Inventory.Findings) != 0 || pub.PublicationID != "" || pub.ProviderAttemptID != "" || run.ProviderRunID != "" || pub.Trigger.CommentID != 0 {
				t.Fatal("malformed marker fabricated publication or completion", run)
			}
			if len(pub.Roots) != 1 || pub.Roots[0].ID != f.snapshot.ReviewComments[0].ID || pub.Roots[0].Body != f.snapshot.ReviewComments[0].Body {
				t.Fatal("actual provider root lost")
			}
			if !reflect.DeepEqual(got, externalDiscover(t, f)) || !reflect.DeepEqual(*review, want) {
				t.Fatal("repeat changed stable identity or raw source")
			}
		})
	}
}

func TestExternalHostedBadMarkerCannotJoinOrCompleteValidBatches(t *testing.T) {
	f := externalFixture(t)
	marker, found, malformed := publicationBatchMarker(f.snapshot.Reviews[0].Body)
	if !found || malformed {
		t.Fatal("invalid fixture marker")
	}
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
	bad := f.snapshot.Reviews[0]
	bad.ID++
	bad.SubmittedAt = bad.SubmittedAt.Add(time.Second)
	badMarker := strings.ReplaceAll(publicationTestMarker(2, 2), publicationTestID, marker.publication)
	bad.Body = badMarker + "\n" + badMarker
	f.snapshot.Reviews = append(f.snapshot.Reviews, bad)
	got := externalDiscover(t, f)
	if len(got) != 2 {
		t.Fatalf("malformed batch was lost or joined an unproven publication: %d", len(got))
	}
	if got[0].SourceID != "publication:"+marker.publication || got[0].Inventory.Complete || got[0].Inventory.Reason != "missing_publication_batches" || !reflect.DeepEqual(got[0].ReviewIDs, []int64{f.snapshot.Reviews[0].ID}) {
		t.Fatal("bad review completed/changed valid partial group", got[0])
	}
	if got[1].SourceID != "review:"+strconv.FormatInt(bad.ID, 10) || got[1].Inventory.Complete || got[1].Inventory.Reason != "conflicting_publication_evidence" || !reflect.DeepEqual(got[1].ReviewIDs, []int64{bad.ID}) {
		t.Fatal("bad review not independently retained", got[1])
	}
}

func TestExternalHostedMalformedMarkerChatterAndForeignActorsStayIgnored(t *testing.T) {
	for _, kind := range []string{"quoted", "fenced", "html-quoted", "html-pre", "foreign-actor", "pending"} {
		t.Run(kind, func(t *testing.T) {
			f := externalFixture(t)
			body := "<!-- coderabbit-review-publication malformed -->"
			switch kind {
			case "quoted":
				body = "> " + body
			case "fenced":
				body = "```\n" + body + "\n```"
			case "html-quoted":
				body = "<blockquote>\n" + body + "\n</blockquote>"
			case "html-pre":
				body = "<pre>\n" + body + "\n</pre>"
			case "foreign-actor":
				f.snapshot.Reviews[0].Author.ID++
			case "pending":
				f.snapshot.Reviews[0].State = "pending"
				f.snapshot.Reviews[0].SubmittedAt = time.Time{}
			}
			f.snapshot.Reviews[0].Body = body
			if got := externalDiscover(t, f); len(got) != 0 {
				t.Fatal("unrecognized chatter became provider source", got)
			}
		})
	}
}
