package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
)

func standingHeadingBody(body string) string {
	start := strings.Index(body, standingRecentStart) + len(standingRecentStart)
	end := strings.Index(body[start:], standingRecentEnd) + start
	recent := body[start:end]
	at := strings.Index(recent, "<details>")
	return body[:start] + recent[:at] + "<h6>\n" + strings.TrimRight(recent[at:], "\n") + "\n</h6>\n" + body[end:]
}

func TestHostedStandingHeadingRetainsPositiveComparison(t *testing.T) {
	f, baseline, base := positiveStandingFixture(t)
	f.snapshot.IssueComments[1].Body = standingHeadingBody(decoratedStandingBody(f.snapshot.IssueComments[1].Body, true))
	before, _ := json.Marshal(f.snapshot)
	prior, _ := json.Marshal(baseline)
	inventory := hfWant(t, f, true, 1)
	got, ok := ResolveHostedStandingComparison(f.snapshot, baseline, inventory.Publication, f.poster, f.bot, f.intent.HeadSHA, base, base)
	if !ok || got != base {
		t.Fatal("heading presentation lost exact positive comparison", got, ok)
	}
	if _, ok := parseStandingZero(f.snapshot.IssueComments[1].Body); ok {
		t.Fatal("positive heading metadata invented zero inventory")
	}
	after, _ := json.Marshal(f.snapshot)
	again, _ := json.Marshal(baseline)
	if string(before) != string(after) || string(prior) != string(again) {
		t.Fatal("heading parser changed immutable source")
	}
}

func TestHostedStandingHeadingRetainsExplicitZeroInventory(t *testing.T) {
	s, baseline, intent, poster, bot, earliest := annotationBaselineFixture(t)
	s.IssueComments[1].Body = standingHeadingBody(decoratedStandingBody(s.IssueComments[1].Body, true))
	before, _ := json.Marshal(s)
	got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
	if err != nil || !got.Complete || got.Publication.StandingZero == nil || got.Publication.StandingZero.SelectedFiles != 2 || len(got.Findings) != 0 {
		t.Fatal("heading presentation lost explicit zero inventory", got, err)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("heading parser rewrote zero source")
	}
}

func TestHostedStandingHeadingRejectsUnsupportedWrappers(t *testing.T) {
	zero := standingHeadingBody(decoratedStandingBody(standingTestBody(strings.Repeat("c", 40), strings.Repeat("b", 40)), true))
	positive := strings.Replace(zero, "No actionable comments were generated in the recent review.\n", "", 1)
	if _, ok := parseStandingMetadata(zero, true); !ok {
		t.Fatal("unmodified zero fixture must parse before wrapper mutations")
	}
	if _, ok := parseStandingMetadata(positive, false); !ok {
		t.Fatal("unmodified positive fixture must parse before wrapper mutations")
	}
	for name, change := range map[string]func(string) string{
		"missing close": func(s string) string { return strings.Replace(s, "</h6>\n", "", 1) },
		"missing open":  func(s string) string { return strings.Replace(s, "<h6>\n", "", 1) },
		"wrong close":   func(s string) string { return strings.Replace(s, "</h6>", "</h5>", 1) },
		"different heading": func(s string) string {
			return strings.ReplaceAll(strings.ReplaceAll(s, "<h6>", "<h5>"), "</h6>", "</h5>")
		},
		"attributes": func(s string) string { return strings.Replace(s, "<h6>", `<h6 class="metadata">`, 1) },
		"nested": func(s string) string {
			return strings.Replace(strings.Replace(s, "<h6>", "<h6>\n<h6>", 1), "</h6>", "</h6>\n</h6>", 1)
		},
		"empty sibling": func(s string) string { return strings.Replace(s, "</h6>", "</h6>\n<h6>\n</h6>", 1) },
		"hidden":        func(s string) string { return "<!--\n" + s + "\n-->" },
		"quoted":        func(s string) string { return "> " + strings.ReplaceAll(s, "\n", "\n> ") },
		"fenced":        func(s string) string { return "```html\n" + s + "\n```" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseStandingMetadata(change(zero), true); ok {
				t.Fatal("unsupported heading supplied zero proof")
			}
			if _, ok := parseStandingMetadata(change(positive), false); ok {
				t.Fatal("unsupported heading supplied positive comparison metadata")
			}
		})
	}
}

func TestHostedStandingHeadingPreservesAttributionGuards(t *testing.T) {
	for _, change := range []string{"foreign source", "missing baseline", "missing acknowledgment", "wrong head"} {
		t.Run(change, func(t *testing.T) {
			f, baseline, base := positiveStandingFixture(t)
			f.snapshot.IssueComments[1].Body = standingHeadingBody(f.snapshot.IssueComments[1].Body)
			switch change {
			case "foreign source":
				f.snapshot.IssueComments[1].Author.ID++
			case "missing baseline":
				baseline.IssueComments = []IssueComment{}
			case "missing acknowledgment":
				f.snapshot.IssueComments = f.snapshot.IssueComments[:2]
			case "wrong head":
				f.snapshot.IssueComments[1].Body = strings.ReplaceAll(f.snapshot.IssueComments[1].Body, f.intent.HeadSHA, strings.Repeat("e", 40))
			}
			inventory := f.extract(t)
			if _, ok := ResolveHostedStandingComparison(f.snapshot, baseline, inventory.Publication, f.poster, f.bot, f.intent.HeadSHA, base, base); ok {
				t.Fatal("heading presentation weakened original attribution")
			}
		})
	}
}
