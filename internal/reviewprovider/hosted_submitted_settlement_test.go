package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func settledSubmissionFixture(t *testing.T) (hfFixture, HostedPublicationSnapshot) {
	t.Helper()
	f := submittedTestFixture(t)
	trigger := f.snapshot.IssueComments[0].CreatedAt
	f.snapshot.IssueComments[1].UpdatedAt = trigger.Add(5 * time.Second)
	f.snapshot.ObservedAt = trigger.Add(6 * time.Second)
	for i := range f.snapshot.ReviewComments {
		f.snapshot.ReviewComments[i].UpdatedAt = trigger.Add(3 * time.Second)
	}
	b := HostedPublicationSnapshot{Complete: true, ObservedAt: trigger.Add(-time.Second), IssueComments: []IssueComment{}, Reviews: []HostedReview{}, ReviewComments: []ReviewComment{}}
	return f, b
}

func TestHostedSubmittedSettlementRequiresOriginalBaseline(t *testing.T) {
	f, b := settledSubmissionFixture(t)
	before, _ := json.Marshal(f.snapshot)
	if got := f.extract(t); got.Complete || got.Reason != "hosted_finished_inventory_unproven" {
		t.Fatal("strict path changed", got)
	}
	got, err := ExtractHostedFindingsWithBaseline(f.snapshot, b, f.intent, f.poster, f.bot, f.earliest)
	if err != nil || !got.Complete || len(got.Findings) != 2 || got.Publication.SubmittedReview == nil || got.Publication.SubmittedReview.ProviderRunID != submittedTestRun {
		t.Fatal("settlement not proven", got, err)
	}
	after, _ := json.Marshal(f.snapshot)
	if string(before) != string(after) || got.Publication.Roots[0].UpdatedAt != f.snapshot.ReviewComments[0].UpdatedAt || got.Publication.FinishedAt != f.snapshot.IssueComments[1].UpdatedAt {
		t.Fatal("settlement changed original evidence")
	}
}

func TestHostedSubmittedSettlementAmbiguitiesStayUnproven(t *testing.T) {
	cases := map[string]func(*hfFixture, *HostedPublicationSnapshot){
		"incomplete-baseline":    func(_ *hfFixture, b *HostedPublicationSnapshot) { b.Complete = false },
		"nil-baseline-roots":     func(_ *hfFixture, b *HostedPublicationSnapshot) { b.ReviewComments = nil },
		"nil-baseline-reviews":   func(_ *hfFixture, b *HostedPublicationSnapshot) { b.Reviews = nil },
		"nil-baseline-issues":    func(_ *hfFixture, b *HostedPublicationSnapshot) { b.IssueComments = nil },
		"baseline-at-trigger":    func(f *hfFixture, b *HostedPublicationSnapshot) { b.ObservedAt = f.snapshot.IssueComments[0].CreatedAt },
		"invalid-baseline-clock": func(_ *hfFixture, b *HostedPublicationSnapshot) { b.ObservedAt = time.Time{} },
		"preexisting-review": func(f *hfFixture, b *HostedPublicationSnapshot) {
			r := f.snapshot.Reviews[0]
			r.SubmittedAt = b.ObservedAt
			b.Reviews = append(b.Reviews, r)
		},
		"preexisting-root": func(f *hfFixture, b *HostedPublicationSnapshot) {
			r := f.snapshot.ReviewComments[0]
			r.CreatedAt = b.ObservedAt
			r.UpdatedAt = b.ObservedAt
			b.ReviewComments = append(b.ReviewComments, r)
		},
		"reused-run-issue": func(f *hfFixture, b *HostedPublicationSnapshot) {
			b.IssueComments = append(b.IssueComments, IssueComment{ID: 900, Author: f.bot, Body: submittedTestRun, CreatedAt: b.ObservedAt, UpdatedAt: b.ObservedAt})
		},
		"reused-run-review": func(f *hfFixture, b *HostedPublicationSnapshot) {
			r := f.snapshot.Reviews[0]
			r.ID = 900
			r.SubmittedAt = b.ObservedAt
			b.Reviews = append(b.Reviews, r)
		},
		"reused-run-root": func(f *hfFixture, b *HostedPublicationSnapshot) {
			r := f.snapshot.ReviewComments[0]
			r.ID = 900
			r.ReviewID = 901
			r.Body = submittedTestRun
			r.CreatedAt = b.ObservedAt
			r.UpdatedAt = b.ObservedAt
			b.ReviewComments = append(b.ReviewComments, r)
		},
		"edit-at-ack": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments[0].UpdatedAt = f.snapshot.IssueComments[1].UpdatedAt
		},
		"edit-after-ack": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments[0].UpdatedAt = f.snapshot.ObservedAt
		},
		"root-after-submission": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments[0].CreatedAt = f.snapshot.ReviewComments[0].UpdatedAt
		},
		"root-at-trigger": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments[0].CreatedAt = f.snapshot.IssueComments[0].CreatedAt
		},
		"wrong-original-head": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments[0].OriginalHeadSHA = strings.Repeat("e", 40)
		},
		"wrong-root-actor": func(f *hfFixture, _ *HostedPublicationSnapshot) { f.snapshot.ReviewComments[0].Author.ID++ },
		"missing-root": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments = f.snapshot.ReviewComments[:1]
		},
		"extra-root": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			r := f.snapshot.ReviewComments[0]
			r.ID = 900
			f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, r)
		},
		"orphan-root": func(f *hfFixture, _ *HostedPublicationSnapshot) { f.snapshot.ReviewComments[0].ReviewID = 900 },
		"unknown-root-body": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments[0].Body = "Unsupported fresh finding"
		},
		"unselected-path": func(f *hfFixture, _ *HostedPublicationSnapshot) { f.snapshot.ReviewComments[0].Path = "other.go" },
		"missing-ack": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.IssueComments = f.snapshot.IssueComments[:1]
		},
		"wrong-ack-actor": func(f *hfFixture, _ *HostedPublicationSnapshot) { f.snapshot.IssueComments[1].Author.ID++ },
		"duplicate-ack": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			a := f.snapshot.IssueComments[1]
			a.ID = 900
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, a)
		},
		"ack-before-submit": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.IssueComments[1].UpdatedAt = f.snapshot.Reviews[0].SubmittedAt
		},
		"competing-review": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			r := f.snapshot.Reviews[0]
			r.ID = 900
			f.snapshot.Reviews = append(f.snapshot.Reviews, r)
		},
		"pending-review": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			r := f.snapshot.Reviews[0]
			r.ID = 900
			r.State = "pending"
			r.SubmittedAt = time.Time{}
			f.snapshot.Reviews = append(f.snapshot.Reviews, r)
		},
		"competing-command": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			c := f.snapshot.IssueComments[0]
			c.ID = 900
			c.CreatedAt = c.CreatedAt.Add(4 * time.Second)
			c.UpdatedAt = c.CreatedAt
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, c)
		},
		"incomplete-current": func(f *hfFixture, _ *HostedPublicationSnapshot) { f.snapshot.Complete = false },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, b := settledSubmissionFixture(t)
			change(&f, &b)
			got, err := ExtractHostedFindingsWithBaseline(f.snapshot, b, f.intent, f.poster, f.bot, f.earliest)
			if err == nil && got.Complete {
				t.Fatal("ambiguous settlement credited", got)
			}
		})
	}
}
