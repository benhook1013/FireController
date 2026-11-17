package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func positiveStandingFixture(t *testing.T) (hfFixture, HostedPublicationSnapshot, string) {
	t.Helper()
	f := omittedInlineFixture(t)
	base := strings.Repeat("b", 40)
	at := f.snapshot.IssueComments[0].CreatedAt
	body := strings.Replace(standingTestBody(f.intent.HeadSHA, base), "No actionable comments were generated in the recent review. 🎉\n", "", 1)
	body = strings.Replace(body, "pkg/alpha.go", "engine/work.go", 1)
	old := IssueComment{ID: 601, Body: "Previous synthetic provider report.", Author: f.bot, CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Minute)}
	baseline := HostedPublicationSnapshot{Complete: true, ObservedAt: at.Add(-time.Second), IssueComments: []IssueComment{old}, Reviews: []HostedReview{}, ReviewComments: []ReviewComment{}}
	current := old
	current.Body = body
	current.UpdatedAt = at.Add(2 * time.Second)
	ack := IssueComment{ID: 602, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\nFull review finished.\n</details>", Author: f.bot, CreatedAt: at.Add(time.Second), UpdatedAt: at.Add(2 * time.Second)}
	f.snapshot.IssueComments = append(f.snapshot.IssueComments, current, ack)
	return f, baseline, base
}
func TestHostedPositiveStandingComparisonRetainsOriginalSources(t *testing.T) {
	f, baseline, base := positiveStandingFixture(t)
	before, _ := json.Marshal(f.snapshot)
	prior, _ := json.Marshal(baseline)
	inventory := hfWant(t, f, true, 1)
	if _, ok := ResolveHostedComparison(inventory.Publication, f.intent.HeadSHA, base, base); ok {
		t.Fatal("omitted scope unexpectedly proven by batch")
	}
	got, ok := ResolveHostedStandingComparison(f.snapshot, baseline, inventory.Publication, f.poster, f.bot, f.intent.HeadSHA, base, base)
	if !ok || got != base {
		t.Fatal("positive source metadata refused", got, ok)
	}
	after, _ := json.Marshal(f.snapshot)
	again, _ := json.Marshal(baseline)
	if string(before) != string(after) || string(prior) != string(again) {
		t.Fatal("comparison changed raw source")
	}
	if _, zero := parseStandingZero(f.snapshot.IssueComments[1].Body); zero {
		t.Fatal("positive metadata invented zero inventory")
	}
}
func TestHostedPositiveStandingComparisonRefusesUnprovenMetadata(t *testing.T) {
	for name, change := range map[string]func(*hfFixture, *HostedPublicationSnapshot){
		"explicit zero metadata": func(f *hfFixture, b *HostedPublicationSnapshot) {
			f.snapshot.IssueComments[1].Body = strings.Replace(f.snapshot.IssueComments[1].Body, standingRecentStart, standingRecentStart+"\nNo actionable comments were generated in the recent review. 🎉", 1)
		},
		"no ACK": func(f *hfFixture, b *HostedPublicationSnapshot) {
			f.snapshot.IssueComments = f.snapshot.IssueComments[:2]
		},
		"foreign ACK":     func(f *hfFixture, b *HostedPublicationSnapshot) { f.snapshot.IssueComments[2].Author.ID++ },
		"foreign summary": func(f *hfFixture, b *HostedPublicationSnapshot) { f.snapshot.IssueComments[1].Author.ID++ },
		"reused run":      func(f *hfFixture, b *HostedPublicationSnapshot) { b.IssueComments[0].Body = standingTestRun },
		"reused root run": func(f *hfFixture, b *HostedPublicationSnapshot) {
			b.ReviewComments = []ReviewComment{f.snapshot.ReviewComments[0]}
			b.ReviewComments[0].CreatedAt = b.ObservedAt
			b.ReviewComments[0].UpdatedAt = b.ObservedAt
			b.ReviewComments[0].Body = standingTestRun
		},
		"partial baseline":     func(f *hfFixture, b *HostedPublicationSnapshot) { b.Complete = false },
		"missing prior source": func(f *hfFixture, b *HostedPublicationSnapshot) { b.IssueComments = []IssueComment{} },
		"wrong base": func(f *hfFixture, b *HostedPublicationSnapshot) {
			f.snapshot.IssueComments[1].Body = strings.ReplaceAll(f.snapshot.IssueComments[1].Body, strings.Repeat("b", 40), strings.Repeat("d", 40))
		},
		"wrong batch scope": func(f *hfFixture, b *HostedPublicationSnapshot) {
			f.snapshot.Reviews[0].Body += "\n" + hostedScopeDeclaration(strings.Repeat("d", 40), f.intent.HeadSHA)
		},
		"malformed batch scope": func(f *hfFixture, b *HostedPublicationSnapshot) {
			f.snapshot.Reviews[0].Body += "\nReviewing files from an unknown comparison."
		},
		"path mismatch": func(f *hfFixture, b *HostedPublicationSnapshot) {
			f.snapshot.IssueComments[1].Body = strings.Replace(f.snapshot.IssueComments[1].Body, "engine/work.go", "another/work.go", 1)
		},
		"path count mismatch": func(f *hfFixture, b *HostedPublicationSnapshot) {
			f.snapshot.IssueComments[1].Body = strings.Replace(f.snapshot.IssueComments[1].Body, "processing: 2", "processing: 3", 1)
		},
		"quoted info": func(f *hfFixture, b *HostedPublicationSnapshot) {
			f.snapshot.IssueComments[1].Body = "> " + strings.ReplaceAll(f.snapshot.IssueComments[1].Body, "\n", "\n> ")
		},
		"duplicate info": func(f *hfFixture, b *HostedPublicationSnapshot) {
			c := f.snapshot.IssueComments[1]
			c.ID++
			c.ID++
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, c)
		},
		"competing command": func(f *hfFixture, b *HostedPublicationSnapshot) {
			c := f.snapshot.IssueComments[0]
			c.ID = 700
			c.CreatedAt = c.CreatedAt.Add(time.Second)
			c.UpdatedAt = c.CreatedAt
			c.Body = "@coderabbitai review"
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, b, base := positiveStandingFixture(t)
			change(&f, &b)
			inventory := f.extract(t)
			if _, ok := ResolveHostedStandingComparison(f.snapshot, b, inventory.Publication, f.poster, f.bot, f.intent.HeadSHA, base, base); ok {
				t.Fatal("unproven metadata granted comparison")
			}
		})
	}
}

func TestHostedPositiveStandingPreviousReviewLabelRetainsOriginalComparison(t *testing.T) {
	f, baseline, base := positiveStandingFixture(t)
	f.snapshot.IssueComments[1].Body = strings.Replace(f.snapshot.IssueComments[1].Body, "· up to <code>", "· previous review <code>", 1)
	before, _ := json.Marshal(f.snapshot)
	inventory := hfWant(t, f, true, 1)
	got, ok := ResolveHostedStandingComparison(f.snapshot, baseline, inventory.Publication, f.poster, f.bot, f.intent.HeadSHA, base, base)
	if !ok || got != base {
		t.Fatal("historical label lost original comparison", got, ok)
	}
	// Presentation about a previous review cannot establish coverage of a newer head.
	if _, ok := ResolveHostedStandingComparison(f.snapshot, baseline, inventory.Publication, f.poster, f.bot, strings.Repeat("e", 40), base, base); ok {
		t.Fatal("historical label credited later head")
	}
	after, _ := json.Marshal(f.snapshot)
	if string(before) != string(after) {
		t.Fatal("historical presentation proof rewrote source")
	}
}
