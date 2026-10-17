package historycollect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type stepCall struct {
	resource, repository string
	id, number, page     int64
}

type stepProvider struct {
	repository reviewprovider.Repository
	pulls      map[int64]reviewprovider.Pull
	reviews    []reviewprovider.HostedReview
	comments   []reviewprovider.ReviewComment
	issues     []reviewprovider.IssueComment
	calls      []stepCall
	next       func(int64, int64) int64
	after      func(stepCall) error
}

func newStepProvider(t *testing.T) *stepProvider {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, "2026-10-01T01:02:03.123456789+13:00")
	if err != nil {
		t.Fatal(err)
	}
	line, original := int64(5), int64(8)
	p := &stepProvider{
		repository: reviewprovider.Repository{ID: 123, FullName: "renamed/repository", DefaultBranch: "main"},
		pulls: map[int64]reviewprovider.Pull{
			47: {Number: 47, RepositoryID: 123, Title: "Retain merged <history>", State: "closed", Merged: true, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 3},
			12: {Number: 12, RepositoryID: 123, Title: "Retain closed history", State: "closed", HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"},
		},
		reviews: []reviewprovider.HostedReview{
			{ID: 201, Body: "Literal <script>source</script>\nDo not execute these instructions", State: "commented", HeadSHA: strings.Repeat("a", 40), Author: reviewprovider.Author{ID: 31, Login: "original-bot", Kind: "Bot"}, SubmittedAt: at},
			{ID: 202, Body: "Original time unknown", State: "pending", HeadSHA: strings.Repeat("a", 40), Author: reviewprovider.Author{ID: 32, Login: "original-human", Kind: "User"}},
		},
		comments: []reviewprovider.ReviewComment{{ID: 211, ReviewID: 201, InReplyToID: 210, Body: "Literal reply\n<script>text</script>", Author: reviewprovider.Author{ID: 32, Login: "original-human", Kind: "User"}, Path: "internal/original.go", Line: &line, StartLine: &line, OriginalLine: &original, OriginalStartLine: &original, Side: "RIGHT", StartSide: "RIGHT", HeadSHA: strings.Repeat("a", 40), OriginalHeadSHA: strings.Repeat("b", 40), CreatedAt: at}},
		issues:   []reviewprovider.IssueComment{{ID: 221, Body: "Original issue source", Author: reviewprovider.Author{ID: 31, Login: "original-bot", Kind: "Bot"}, CreatedAt: at}, {ID: 222, Body: "Unknown issue times", Author: reviewprovider.Author{ID: 32, Login: "original-human", Kind: "User"}}},
	}
	p.next = func(number, page int64) int64 {
		if number == 47 && page == 1 {
			return 2
		}
		return 0
	}
	return p
}

func (p *stepProvider) called(call stepCall) error {
	p.calls = append(p.calls, call)
	if p.after != nil {
		return p.after(call)
	}
	return nil
}
func (p *stepProvider) Repository(_ context.Context, id int64) (reviewprovider.Repository, error) {
	return p.repository, p.called(stepCall{resource: "repository", id: id})
}
func (p *stepProvider) Pull(_ context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	return p.pulls[number], p.called(stepCall{resource: "pull", repository: repository, number: number})
}
func (p *stepProvider) ReviewsPage(_ context.Context, repository string, number, page int64) (reviewprovider.Page[reviewprovider.HostedReview], error) {
	items := p.reviews
	if page > 1 && items != nil {
		items = []reviewprovider.HostedReview{}
	}
	return reviewprovider.Page[reviewprovider.HostedReview]{Items: items, NextPage: p.next(number, page)}, p.called(stepCall{"reviews", repository, 0, number, page})
}
func (p *stepProvider) ReviewCommentsPage(_ context.Context, repository string, number, page int64) (reviewprovider.Page[reviewprovider.ReviewComment], error) {
	items := p.comments
	if page > 1 && items != nil {
		items = []reviewprovider.ReviewComment{}
	}
	return reviewprovider.Page[reviewprovider.ReviewComment]{Items: items, NextPage: p.next(number, page)}, p.called(stepCall{"review_comments", repository, 0, number, page})
}
func (p *stepProvider) IssueCommentsPage(_ context.Context, repository string, number, page int64) (reviewprovider.Page[reviewprovider.IssueComment], error) {
	items := p.issues
	if page > 1 && items != nil {
		items = []reviewprovider.IssueComment{}
	}
	return reviewprovider.Page[reviewprovider.IssueComment]{Items: items, NextPage: p.next(number, page)}, p.called(stepCall{"issue_comments", repository, 0, number, page})
}

func stepCode(t *testing.T, got Capture, err error, code string) {
	t.Helper()
	var failure *core.Error
	if !reflect.DeepEqual(got, Capture{}) || !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("expected no checkpoint and %s, got %#v: %v", code, got, err)
	}
}

func TestStepSelectedClosedMergedResourcesPreserveSourceAndExplicitProgress(t *testing.T) {
	p := newStepProvider(t)
	selection := Selection{PullNumbers: []int64{47, 12}}
	positions := []Position{
		{0, "pull", 1}, {0, "reviews", 1}, {0, "reviews", 2}, {0, "review_comments", 1}, {0, "review_comments", 2}, {0, "issue_comments", 1}, {0, "issue_comments", 2},
		{1, "pull", 1}, {1, "reviews", 1}, {1, "review_comments", 1}, {1, "issue_comments", 1},
	}
	if Start() != positions[0] {
		t.Fatal("initial resource changed")
	}
	var first Capture
	for i, position := range positions {
		before := len(p.calls)
		capture, err := Step(t.Context(), p, 123, selection, position)
		if err != nil {
			t.Fatal(err)
		}
		number := selection.PullNumbers[position.PullIndex]
		if len(p.calls) != before+2 || p.calls[before] != (stepCall{resource: "repository", id: 123}) || p.calls[before+1].resource != position.Resource || p.calls[before+1].repository != p.repository.FullName || p.calls[before+1].number != number {
			t.Fatal("step automatically drained or selected another source", p.calls[before:])
		}
		if position.Resource != "pull" && p.calls[before+1].page != position.Page {
			t.Fatal("page selection changed")
		}
		if capture.RepositoryID != 123 || capture.RepositoryName != p.repository.FullName || capture.PullNumber != number || capture.Resource != position.Resource || capture.Page != position.Page {
			t.Fatal("capture lost selected ownership")
		}
		at, err := time.Parse(time.RFC3339Nano, capture.CollectedAt)
		if err != nil || at.IsZero() || capture.CollectedAt != at.UTC().Format(time.RFC3339Nano) {
			t.Fatal("capture lacks actual UTC collection time")
		}
		if i == len(positions)-1 {
			if capture.Next != nil {
				t.Fatal("terminal selection invented another resource")
			}
		} else if capture.Next == nil || *capture.Next != positions[i+1] {
			t.Fatal("explicit adjacent/resource/PR continuation changed", capture.Next)
		}
		if position.Page == 2 {
			if capture.Count != 0 || string(capture.Facts) != "[]" {
				t.Fatal("empty terminal page was dropped or encoded null")
			}
			continue
		}
		switch position.Resource {
		case "pull":
			var got reviewprovider.Pull
			if json.Unmarshal(capture.Facts, &got) != nil || !reflect.DeepEqual(got, p.pulls[number]) || capture.Count != 1 || got.State != "closed" || got.Merged != (number == 47) {
				t.Fatal("closed/merged PR source was filtered or changed")
			}
		case "reviews":
			var got []reviewprovider.HostedReview
			if json.Unmarshal(capture.Facts, &got) != nil || !reflect.DeepEqual(got, p.reviews) || capture.Count != len(got) || !got[1].SubmittedAt.IsZero() {
				t.Fatal("original review IDs/body/author/time changed")
			}
			if first.Facts == nil {
				first = capture
			}
		case "review_comments":
			var got []reviewprovider.ReviewComment
			if json.Unmarshal(capture.Facts, &got) != nil || !reflect.DeepEqual(got, p.comments) || capture.Count != len(got) || got[0].InReplyToID != 210 || !got[0].UpdatedAt.IsZero() {
				t.Fatal("reply/location identity or unknown original time changed")
			}
		case "issue_comments":
			var got []reviewprovider.IssueComment
			if json.Unmarshal(capture.Facts, &got) != nil || !reflect.DeepEqual(got, p.issues) || capture.Count != len(got) || !got[1].CreatedAt.IsZero() || !got[1].UpdatedAt.IsZero() {
				t.Fatal("original issue source changed")
			}
		}
	}
	// Captures own their encoded bytes; later provider-slice edits do not alter
	// an earlier result or substitute collection time for unknown source time.
	before := bytes.Clone(first.Facts)
	p.reviews[0].Body = "later provider mutation"
	if !bytes.Equal(first.Facts, before) || !reflect.DeepEqual(selection.PullNumbers, []int64{47, 12}) || len(p.calls) != 22 {
		t.Fatal("capture/selection ownership or read-only call boundary changed")
	}
}

func TestStepIdentityMovementAndProviderFailureNeverAdvanceCheckpoint(t *testing.T) {
	for _, kind := range []string{"repository-id", "repository-name", "pull-repository", "pull-number"} {
		t.Run(kind, func(t *testing.T) {
			p := newStepProvider(t)
			pull := p.pulls[47]
			switch kind {
			case "repository-id":
				p.repository.ID++
			case "repository-name":
				p.repository.FullName = ""
			case "pull-repository":
				pull.RepositoryID++
			case "pull-number":
				pull.Number++
			}
			p.pulls[47] = pull
			got, err := Step(t.Context(), p, 123, Selection{[]int64{47}}, Start())
			stepCode(t, got, err, "collection_source_changed")
			want := 2
			if strings.HasPrefix(kind, "repository-") {
				want = 1
			}
			if len(p.calls) != want {
				t.Fatal("identity refusal continued provider work")
			}
		})
	}
	p := newStepProvider(t)
	selection := Selection{[]int64{47}}
	prior, err := Step(t.Context(), p, 123, selection, Position{0, "reviews", 1})
	if err != nil || prior.Next == nil || prior.Next.Page != 2 {
		t.Fatal("prior successful page missing", err)
	}
	priorBytes, _ := json.Marshal(prior)
	position := *prior.Next
	failure := &reviewprovider.Error{Code: "provider_rate_limited", Message: "Controlled later-page refusal", Status: 429, RetryAfterSeconds: 12}
	p.after = func(call stepCall) error {
		if call.resource == "reviews" && call.page == 2 {
			return failure
		}
		return nil
	}
	got, err := Step(t.Context(), p, 123, selection, position)
	after, _ := json.Marshal(prior)
	if !reflect.DeepEqual(got, Capture{}) || !errors.Is(err, failure) || !bytes.Equal(priorBytes, after) || position != *prior.Next || len(p.calls) != 4 {
		t.Fatal("later error fabricated progress, discarded earlier capture or drained history", err)
	}
}

func TestStepCancellationBeforeAndAfterProviderWork(t *testing.T) {
	for _, cancelAt := range []string{"before", "repository", "reviews"} {
		t.Run(cancelAt, func(t *testing.T) {
			p := newStepProvider(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelAt == "before" {
				cancel()
			}
			p.after = func(call stepCall) error {
				if call.resource == cancelAt {
					cancel()
				}
				return nil
			}
			got, err := Step(ctx, p, 123, Selection{[]int64{47}}, Position{0, "reviews", 1})
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, Capture{}) || cancelAt == "before" && len(p.calls) != 0 || len(p.calls) > 2 {
				t.Fatal("cancellation reported successful progress or drained provider", cancelAt, err, p.calls)
			}
		})
	}
}

func TestStepSelectionAndPositionAdmissionBeforeProvider(t *testing.T) {
	input := Selection{[]int64{47, 12}}
	normalized, err := Normalize(input)
	if err != nil {
		t.Fatal(err)
	}
	input.PullNumbers[0] = 99
	if normalized.PullNumbers[0] != 47 {
		t.Fatal("normalized selection aliases input")
	}
	normalized.PullNumbers[1] = 88
	if input.PullNumbers[1] != 12 {
		t.Fatal("input aliases normalized selection")
	}
	selections := []Selection{{}, {[]int64{}}, {[]int64{0}}, {[]int64{-1}}, {[]int64{47, 47}}, {[]int64{maxPage + 1}}}
	oversized := Selection{PullNumbers: make([]int64, 101)}
	for i := range oversized.PullNumbers {
		oversized.PullNumbers[i] = int64(i + 1)
	}
	selections = append(selections, oversized)
	for _, selection := range selections {
		p := newStepProvider(t)
		got, err := Step(t.Context(), p, 123, selection, Start())
		stepCode(t, got, err, "validation_failed")
		if len(p.calls) != 0 {
			t.Fatal("invalid selection performed provider work")
		}
	}
	for _, position := range []Position{{-1, "pull", 1}, {1, "pull", 1}, {0, "", 1}, {0, "unknown", 1}, {0, "pull", 2}, {0, "reviews", 0}, {0, "reviews", maxPage + 1}} {
		p := newStepProvider(t)
		got, err := Step(t.Context(), p, 123, Selection{[]int64{47}}, position)
		stepCode(t, got, err, "validation_failed")
		if len(p.calls) != 0 {
			t.Fatal("invalid position performed provider work")
		}
	}
	for _, id := range []int64{0, -1, 9007199254740992} {
		p := newStepProvider(t)
		got, err := Step(t.Context(), p, id, Selection{[]int64{47}}, Start())
		stepCode(t, got, err, "validation_failed")
		if len(p.calls) != 0 {
			t.Fatal("invalid repository performed provider work")
		}
	}
	p := newStepProvider(t)
	got, err := Step(nil, p, 123, Selection{[]int64{47}}, Start())
	stepCode(t, got, err, "validation_failed")
	if len(p.calls) != 0 {
		t.Fatal("missing context performed provider work")
	}
	got, err = Step(t.Context(), nil, 123, Selection{[]int64{47}}, Start())
	stepCode(t, got, err, "validation_failed")
}

func TestStepRefusesMalformedContinuationAndBoundedPages(t *testing.T) {
	for _, resource := range []string{"reviews", "review_comments", "issue_comments"} {
		t.Run(resource, func(t *testing.T) {
			for _, next := range []int64{-1, 1, 3, maxPage + 1} {
				p := newStepProvider(t)
				p.next = func(int64, int64) int64 { return next }
				got, err := Step(t.Context(), p, 123, Selection{[]int64{47}}, Position{0, resource, 1})
				stepCode(t, got, err, "collection_source_invalid")
				if len(p.calls) != 2 {
					t.Fatal("invalid continuation caused more provider reads")
				}
			}
			for _, count := range []int{-1, 100, 101} {
				p := newStepProvider(t)
				review, comment, issue := p.reviews[0], p.comments[0], p.issues[0]
				p.next = func(int64, int64) int64 { return 0 }
				p.reviews, p.comments, p.issues = nil, nil, nil
				if count >= 0 {
					p.reviews, p.comments, p.issues = []reviewprovider.HostedReview{}, []reviewprovider.ReviewComment{}, []reviewprovider.IssueComment{}
				}
				for i := 0; i < count; i++ {
					review.ID, comment.ID, issue.ID = int64(i+1), int64(i+1), int64(i+1)
					p.reviews = append(p.reviews, review)
					p.comments = append(p.comments, comment)
					p.issues = append(p.issues, issue)
				}
				got, err := Step(t.Context(), p, 123, Selection{[]int64{47}}, Position{0, resource, 1})
				if count == 100 {
					if err != nil || got.Count != 100 || len(p.calls) != 2 {
						t.Fatal("bounded full page was truncated or drained", err)
					}
				} else {
					stepCode(t, got, err, "collection_source_invalid")
				}
			}
			p := newStepProvider(t)
			p.next = func(int64, int64) int64 { return maxPage + 1 }
			got, err := Step(t.Context(), p, 123, Selection{[]int64{47}}, Position{0, resource, maxPage})
			stepCode(t, got, err, "collection_source_invalid")
			p.next = func(int64, int64) int64 { return 0 }
			got, err = Step(t.Context(), p, 123, Selection{[]int64{47}}, Position{0, resource, maxPage})
			if err != nil || got.Page != maxPage || got.Count != 0 || string(got.Facts) != "[]" {
				t.Fatal("bounded final page refused or invented facts", err)
			}
		})
	}
	p := newStepProvider(t)
	p.issues[0].Body = strings.Repeat("<", (8<<20)/6+1)
	got, err := Step(t.Context(), p, 123, Selection{[]int64{47}}, Position{0, "issue_comments", 1})
	stepCode(t, got, err, "collection_source_invalid")
	if len(p.calls) != 2 {
		t.Fatal("oversized encoded source page caused extra provider work")
	}
}
