package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const externalZeroRun = "77777777-7777-4777-8777-777777777777"

func externalCurrentSubmission(head string, omitted bool) string {
	body := submittedCurrentBody(head)
	start, end := strings.Index(body, "> [!CAUTION]"), strings.Index(body, "\n---")
	body = body[:start] + body[end:]
	body = strings.Replace(body, "Prompt to fix review comments</summary>", "Prompt to fix all issues</summary>", 1)
	body = strings.Replace(body, "</details>\n\n---\n\n<details>", "</details>\n\n<h6>\n<details>", 1)
	body = strings.Replace(body, "</details>\n\n<!-- This is an auto-generated comment by CodeRabbit for review status -->", "</details>\n</h6>\n\n<!-- This is an auto-generated comment by CodeRabbit for review status -->", 1)
	if omitted {
		body = strings.Replace(body, "**Actionable comments posted: 1**", "", 1)
		start, end := strings.Index(body, "<!-- autofix_checkbox_start -->"), strings.Index(body, "<!-- autofix_checkbox_end -->")
		body = body[:start] + body[end+len("<!-- autofix_checkbox_end -->"):]
		body = strings.Replace(body, submittedTestRun, externalZeroRun, 1)
	}
	return body
}

func externalCurrentZeroFixture(t *testing.T) hfFixture {
	t.Helper()
	f := submittedCurrentFixture(t)
	f.snapshot.Reviews[0].Body = externalCurrentSubmission(f.intent.HeadSHA, true)
	f.snapshot.ReviewComments = []ReviewComment{}
	f.snapshot.IssueComments[0].Body = "@coderabbitai full review"
	return f
}

func TestExternalHostedCurrentSubmissionsKeepDistinctSameHeadRuns(t *testing.T) {
	f := externalCurrentZeroFixture(t)
	positive := submittedCurrentFixture(t)
	old := positive.snapshot.Reviews[0]
	old.ID += 10
	old.SubmittedAt = old.SubmittedAt.Add(-time.Hour)
	old.Body = externalCurrentSubmission(old.HeadSHA, false)
	root := positive.snapshot.ReviewComments[0]
	root.ReviewID, root.CreatedAt, root.UpdatedAt = old.ID, old.SubmittedAt.Add(-time.Second), old.SubmittedAt.Add(time.Second)
	f.snapshot.ReviewComments = []ReviewComment{root}
	f.snapshot.Reviews = append(f.snapshot.Reviews, old)
	before := fixtureJSON(t, f)
	got := externalDiscover(t, f)
	if len(got) != 2 || got[0].SourceID != "run:"+submittedTestRun || got[1].SourceID != "run:"+externalZeroRun {
		t.Fatal("distinct same-head runs were lost or combined")
	}
	for i, run := range got {
		review := f.snapshot.Reviews[1-i]
		if !run.Inventory.Complete || !run.FinishedAt.Equal(review.SubmittedAt) || run.HeadSHA != review.HeadSHA || !reflect.DeepEqual(run.ReviewIDs, []int64{review.ID}) || run.Inventory.Publication.Batches[0].Body != review.Body || run.Inventory.Publication.SubmittedReview != nil || run.Inventory.Publication.StandingZero != nil || run.TriggerCommentID != 0 || run.NativeOperationID != "" {
			t.Fatal("source publication or passive attribution changed", i, run.Inventory.Reason)
		}
		if _, _, _, valid := parseSubmittedReview(review.Body, review.HeadSHA); valid {
			t.Fatal("archive presentation granted native submitted proof")
		}
	}
	if len(got[0].Inventory.Findings) != 1 || len(got[1].Inventory.Findings) != 0 {
		t.Fatal("incorrect positive or empty finding inventory")
	}
	finding := got[0].Inventory.Findings[0]
	if finding.CommentID != root.ID || finding.ReviewID != old.ID || finding.Path != root.Path || !strings.Contains(root.Body, finding.Detail) {
		t.Fatal("original attached finding provenance or detail changed")
	}
	if !reflect.DeepEqual(before, fixtureJSON(t, f)) || !reflect.DeepEqual(got, externalDiscover(t, f)) {
		t.Fatal("discovery changed the original snapshot or repeated result")
	}
}

func TestExternalHostedCurrentMarkedSummaryRemainsArchiveOnly(t *testing.T) {
	f := submittedCurrentFixture(t)
	review := &f.snapshot.Reviews[0]
	review.Body = externalCurrentSubmission(review.HeadSHA, false) + "\n\n" + publicationTestMarker(1, 1)
	for _, shape := range []string{"supported", "unknown prompt", "unclosed heading", "unrelated heading"} {
		t.Run(shape, func(t *testing.T) {
			review := *review
			switch shape {
			case "unknown prompt":
				review.Body = strings.Replace(review.Body, "Prompt to fix all issues", "Other prompt", 1)
			case "unclosed heading":
				review.Body = strings.Replace(review.Body, "</h6>", "", 1)
			case "unrelated heading":
				review.Body = strings.Replace(review.Body, " Review info</summary>", " Example</summary>", 1)
			}
			f := f
			f.snapshot.Reviews = []HostedReview{review}
			got := externalDiscover(t, f)
			if len(got) != 1 || got[0].Inventory.Complete != (shape == "supported") || len(got[0].Inventory.Findings) != 1 || got[0].Inventory.Publication.Batches[0].Body != review.Body {
				t.Fatal("marked archive summary misclassified or rewrote source")
			}
			if decodeHostedFindings(got[0].Inventory.Publication).Complete {
				t.Fatal("archive summary also passed native inventory decoding")
			}
		})
	}
}

func TestExternalHostedOmittedCountRequiresCompleteTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func(*hfFixture)
		unrecognized bool
	}{
		{"missing reply", func(f *hfFixture) { f.snapshot.IssueComments = f.snapshot.IssueComments[:1] }, false},
		{"failed reply", func(f *hfFixture) {
			f.snapshot.IssueComments[1].Body = strings.Replace(f.snapshot.IssueComments[1].Body, "Full review finished.", "Review failed.", 1)
		}, false},
		{"forged reply", func(f *hfFixture) { f.snapshot.IssueComments[1].Author.ID++ }, false},
		{"edited command", func(f *hfFixture) {
			f.snapshot.IssueComments[0].UpdatedAt = f.snapshot.IssueComments[0].CreatedAt.Add(time.Second)
		}, false},
		{"late reply", func(f *hfFixture) { f.snapshot.IssueComments[1].CreatedAt = f.snapshot.Reviews[0].SubmittedAt }, false},
		{"premature finish", func(f *hfFixture) { f.snapshot.IssueComments[1].UpdatedAt = f.snapshot.Reviews[0].SubmittedAt }, false},
		{"duplicate reply", func(f *hfFixture) {
			other := f.snapshot.IssueComments[1]
			other.ID++
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, other)
		}, false},
		{"reused invocation", func(f *hfFixture) {
			other := f.snapshot.IssueComments[1]
			other.ID++
			other.CreatedAt = other.CreatedAt.Add(-time.Hour)
			other.UpdatedAt = other.UpdatedAt.Add(-time.Hour)
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, other)
		}, false},
		{"competing command", func(f *hfFixture) {
			other := f.snapshot.IssueComments[0]
			other.ID += 30
			other.Body = "@coderabbitai review"
			other.CreatedAt = f.snapshot.Reviews[0].SubmittedAt
			other.UpdatedAt = other.CreatedAt
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, other)
		}, false},
		{"competing submission", func(f *hfFixture) {
			other := f.snapshot.Reviews[0]
			other.ID++
			other.Body = ""
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		}, false},
		{"unexpected root", func(f *hfFixture) {
			root := submittedCurrentFixture(t).snapshot.ReviewComments[0]
			f.snapshot.ReviewComments = []ReviewComment{root}
		}, false},
		{"orphan root in window", func(f *hfFixture) {
			root := submittedCurrentFixture(t).snapshot.ReviewComments[0]
			root.ReviewID++
			f.snapshot.ReviewComments = []ReviewComment{root}
		}, false},
		{"unclassified finding section", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "</h6>", "</h6>\n\n"+hfOutsideCategory(1, hfOutside(3, "engine/queue.go", "12-15", "Outside finding")), 1)
		}, false},
		{"unknown prompt", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Prompt to fix all issues", "Unrecognized prompt", 1)
		}, true},
		{"empty acknowledgement review", func(f *hfFixture) { f.snapshot.Reviews[0].Body = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := externalCurrentZeroFixture(t)
			tc.change(&f)
			got := externalDiscover(t, f)
			if tc.unrecognized {
				if len(got) != 0 {
					t.Fatal("unrecognized body invented a run")
				}
				return
			}
			if len(got) != 1 || got[0].SourceID != "run:"+externalZeroRun || got[0].Inventory.Complete {
				t.Fatal("known omitted-count source was dropped or credited")
			}
		})
	}
}

func TestExternalHostedCurrentRootClockAllowanceIsNarrow(t *testing.T) {
	for _, late := range []string{"creation", "update", "unselected path"} {
		t.Run(late, func(t *testing.T) {
			f := submittedCurrentFixture(t)
			review, root := &f.snapshot.Reviews[0], &f.snapshot.ReviewComments[0]
			review.Body = externalCurrentSubmission(review.HeadSHA, false)
			switch late {
			case "creation":
				root.CreatedAt = review.SubmittedAt.Add(time.Second)
				root.UpdatedAt = root.CreatedAt
			case "update":
				root.UpdatedAt = review.SubmittedAt.Add(2 * time.Second)
			case "unselected path":
				root.Path = "other.go"
			}
			got := externalDiscover(t, f)
			if len(got) != 1 || got[0].Inventory.Complete || got[0].Inventory.Reason != "root_binding_mismatch" {
				t.Fatal("new archive presentation waived exact root binding")
			}
		})
	}
}
