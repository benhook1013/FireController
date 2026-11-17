package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func positiveAnnotationFixture(t *testing.T) (hfFixture, HostedPublicationSnapshot) {
	t.Helper()
	f := omittedInlineFixture(t)
	trigger := f.snapshot.IssueComments[0].CreatedAt
	oldHead := strings.Repeat("b", 40)
	oldReview := HostedReview{ID: 801, Body: "<!-- coderabbit-ai-generated:v1 -->\n\n**Actionable comments posted: 1**\n\nOriginal synthetic historical report.", State: "commented", HeadSHA: oldHead, Author: f.bot, SubmittedAt: trigger.Add(-time.Hour)}
	oldRoot := ReviewComment{ID: 802, ReviewID: 801, Body: hfInline(71), Author: f.bot, Path: "engine/prior.go", HeadSHA: oldHead, OriginalHeadSHA: oldHead, CreatedAt: trigger.Add(-time.Hour), UpdatedAt: trigger.Add(-time.Minute)}
	baseline := HostedPublicationSnapshot{Complete: true, ObservedAt: trigger.Add(-time.Second), IssueComments: []IssueComment{}, Reviews: []HostedReview{oldReview}, ReviewComments: []ReviewComment{oldRoot}}
	currentReview := oldReview
	currentReview.Body = strings.Replace(currentReview.Body, "\n\n**Actionable comments posted: 1**", "", 1)
	f.snapshot.Reviews = append(f.snapshot.Reviews, currentReview)
	oldRoot.Body += "\n\n✅ Addressed in commits 1234567 to " + f.intent.HeadSHA[:7]
	oldRoot.UpdatedAt = trigger.Add(time.Second)
	oldRoot.HeadSHA = strings.Repeat("c", 40)
	f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, oldRoot)
	return f, baseline
}

func TestHostedPositiveAnnotatedInventoryPreservesBothSources(t *testing.T) {
	f, b := positiveAnnotationFixture(t)
	before, _ := json.Marshal(f.snapshot)
	prior, _ := json.Marshal(b)
	strict := f.extract(t)
	if strict.Complete || strict.Reason != "invalid_actionable_count" {
		t.Fatal("expected historical edit barrier", strict.Reason)
	}
	got, err := ExtractHostedFindingsWithBaseline(f.snapshot, b, f.intent, f.poster, f.bot, f.earliest)
	if err != nil || !got.Complete || len(got.Findings) != 1 || got.Findings[0].CommentID != 501 || len(got.Publication.Roots) != 1 {
		t.Fatal("exact old annotation blocked positive inventory", got, err)
	}
	after, _ := json.Marshal(f.snapshot)
	later, _ := json.Marshal(b)
	if string(before) != string(after) || string(prior) != string(later) {
		t.Fatal("projection rewrote retained input")
	}
}

func TestHostedPositiveAnnotatedInventoryRefusesUnprovenProjection(t *testing.T) {
	for name, edit := range map[string]func(*hfFixture, *HostedPublicationSnapshot){
		"missing baseline root": func(_ *hfFixture, b *HostedPublicationSnapshot) { b.ReviewComments = []ReviewComment{} },
		"rewritten historical body": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments[1].Body = "Changed content." + f.snapshot.ReviewComments[1].Body
		},
		"changed historical review": func(f *hfFixture, _ *HostedPublicationSnapshot) { f.snapshot.Reviews[1].Body += "Changed content." },
		"annotation after completion": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments[1].UpdatedAt = f.snapshot.ObservedAt
		},
		"fresh orphan": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			r := f.snapshot.ReviewComments[0]
			r.ID = 900
			r.ReviewID = 999
			f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, r)
		},
		"malformed explicit count": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: unknown**\n" + f.snapshot.Reviews[0].Body
		},
		"no positive inventory": func(f *hfFixture, _ *HostedPublicationSnapshot) {
			f.snapshot.ReviewComments = f.snapshot.ReviewComments[1:]
		},
		"incomplete baseline": func(_ *hfFixture, b *HostedPublicationSnapshot) { b.Complete = false },
	} {
		t.Run(name, func(t *testing.T) {
			f, b := positiveAnnotationFixture(t)
			edit(&f, &b)
			got, err := ExtractHostedFindingsWithBaseline(f.snapshot, b, f.intent, f.poster, f.bot, f.earliest)
			if err != nil || got.Complete {
				t.Fatal("unproven change gained inventory", got, err)
			}
		})
	}
}

func TestHistoricalAnnotationReviewCannotDeleteHiddenCountInsteadOfVisibleCount(t *testing.T) {
	baseline := HostedReview{ID: 801, Body: "**Actionable comments posted: 1**\n\n```example\n\n**Actionable comments posted: 1**\n```", State: "commented", HeadSHA: strings.Repeat("b", 40), Author: Author{ID: 202, Login: "synthetic-reviewer", Kind: "Bot"}, SubmittedAt: time.Date(2026, 7, 8, 8, 0, 0, 0, time.UTC)}
	current := baseline
	current.Body = strings.Replace(current.Body, "\n\n**Actionable comments posted: 1**", "", 1)
	if sameHistoricalAnnotationReview(baseline, current) {
		t.Fatal("hidden heading deletion substituted for actual visible header removal")
	}
}
