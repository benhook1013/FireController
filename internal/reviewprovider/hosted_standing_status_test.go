package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const standingTestZeroSentence = "No actionable comments were generated in the recent review."
const standingTestStatusStart = "<!-- recent_review_status_start -->"
const standingTestStatusEnd = "<!-- recent_review_status_end -->"

func standingStatusEnvelope() string {
	return standingTestStatusStart + "\n\n" + standingTestZeroSentence + "\n\n" + standingTestStatusEnd + "\n"
}

func standingStatusBody(body string) string {
	body = strings.Replace(body, standingTestZeroSentence+" 🎉\n", "", 1)
	body = strings.Replace(body, standingTestZeroSentence+"\n", "", 1)
	return standingStatusEnvelope() + body
}

func TestHostedStandingStatusRetainsOriginalZeroAndComparison(t *testing.T) {
	s, baseline, intent, poster, bot, earliest := annotationBaselineFixture(t)
	s.IssueComments[1].Body = standingStatusBody(standingHeadingBody(decoratedStandingBody(s.IssueComments[1].Body, true)))
	before, _ := json.Marshal(s)
	prior, _ := json.Marshal(baseline)
	got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
	if err != nil || !got.Complete || got.Publication.StandingZero == nil || len(got.Findings) != 0 || got.Publication.Trigger.CommentID != s.IssueComments[0].ID || got.Publication.StandingZero.Summary.ID != s.IssueComments[1].ID || got.Publication.StandingZero.ProviderRunID != standingTestRun || got.Publication.StandingZero.SelectedFiles != 2 {
		t.Fatal("separate status lost original attributable zero inventory", got, err)
	}
	base := strings.Repeat("b", 40)
	if actual, ok := ResolveHostedComparison(got.Publication, intent.HeadSHA, base, base); !ok || actual != base {
		t.Fatal("separate status lost original exact comparison", actual, ok)
	}
	if _, ok := parseStandingMetadata(s.IssueComments[1].Body, false); ok {
		t.Fatal("zero status supplied positive-publication metadata")
	}
	after, _ := json.Marshal(s)
	again, _ := json.Marshal(baseline)
	if string(before) != string(after) || string(prior) != string(again) {
		t.Fatal("status parsing changed immutable sources")
	}
}

func TestHostedStandingStatusRejectsUnsupportedClaimsAndVisibility(t *testing.T) {
	metadata := decoratedStandingBody(standingTestBody(strings.Repeat("c", 40), strings.Repeat("b", 40)), true)
	body := standingStatusBody(standingHeadingBody(metadata))
	if _, ok := parseStandingZero(body); !ok {
		t.Fatal("unmodified status fixture must parse before mutations")
	}
	status := standingStatusEnvelope()
	for name, change := range map[string]func(string) string{
		"missing start": func(s string) string { return strings.Replace(s, standingTestStatusStart, "", 1) },
		"missing end":   func(s string) string { return strings.Replace(s, standingTestStatusEnd, "", 1) },
		"empty status":  func(s string) string { return strings.Replace(s, standingTestZeroSentence, "", 1) },
		"unknown claim": func(s string) string { return strings.Replace(s, standingTestZeroSentence, "Review finished.", 1) },
		"contradictory claim": func(s string) string {
			return strings.Replace(s, standingTestZeroSentence, standingTestZeroSentence+"\nOne actionable comment was generated.", 1)
		},
		"duplicate sentence": func(s string) string {
			return strings.Replace(s, standingTestZeroSentence, standingTestZeroSentence+"\n"+standingTestZeroSentence, 1)
		},
		"duplicate block": func(s string) string { return status + s },
		"inline and status claims": func(s string) string {
			return strings.Replace(s, standingRecentStart, standingRecentStart+"\n"+standingTestZeroSentence, 1)
		},
		"malformed start": func(s string) string {
			return strings.Replace(s, standingTestStatusStart, "<!-- recent_review_status_start extra -->", 1)
		},
		"extra close": func(s string) string { return s + "\n" + standingTestStatusEnd },
		"hidden block": func(s string) string {
			return strings.Replace(s, status, "<!--\n"+status+"-->\n", 1)
		},
		"quoted block": func(s string) string {
			return strings.Replace(s, status, "> "+strings.ReplaceAll(status, "\n", "\n> ")+"\n", 1)
		},
		"indented block": func(s string) string {
			return strings.Replace(s, status, "    "+strings.ReplaceAll(status, "\n", "\n    ")+"\n", 1)
		},
		"fenced block": func(s string) string { return strings.Replace(s, status, "```html\n"+status+"```\n", 1) },
		"nested block": func(s string) string {
			return strings.Replace(s, status, "<details>\n<summary>Example</summary>\n"+status+"</details>\n", 1)
		},
		"inside recent metadata": func(s string) string {
			s = strings.Replace(s, status, "", 1)
			return strings.Replace(s, standingRecentStart, standingRecentStart+"\n"+status, 1)
		},
		"hidden sentence": func(s string) string {
			return strings.Replace(s, standingTestZeroSentence, "<!-- "+standingTestZeroSentence+" -->", 1)
		},
		"quoted sentence": func(s string) string {
			return strings.Replace(s, standingTestZeroSentence, "> "+standingTestZeroSentence, 1)
		},
		"nested sentence": func(s string) string {
			return strings.Replace(s, standingTestZeroSentence, "<span>"+standingTestZeroSentence+"</span>", 1)
		},
		"positive inventory": func(s string) string { return s + "\n**Actionable comments posted: 1**" },
		"no coverage": func(s string) string {
			return strings.Replace(s, "final_review_risk_coverage:", "missing_coverage:", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseStandingZero(change(body)); ok {
				t.Fatal("unsupported status supplied zero inventory")
			}
		})
	}
}

func TestHostedStandingStatusPreservesAttributionGuards(t *testing.T) {
	for _, change := range []string{"foreign summary", "missing ACK", "missing baseline", "changed old finding", "new review", "wrong head"} {
		t.Run(change, func(t *testing.T) {
			s, baseline, intent, poster, bot, earliest := annotationBaselineFixture(t)
			s.IssueComments[1].Body = standingStatusBody(s.IssueComments[1].Body)
			switch change {
			case "foreign summary":
				s.IssueComments[1].Author.ID++
			case "missing ACK":
				s.IssueComments = s.IssueComments[:2]
			case "missing baseline":
				baseline.ReviewComments = nil
			case "changed old finding":
				s.ReviewComments[0].Body = "Changed old finding\n" + s.ReviewComments[0].Body
			case "new review":
				s.Reviews = append(s.Reviews, HostedReview{ID: 700, Author: bot, State: "commented", HeadSHA: intent.HeadSHA, Body: "Unknown fresh review result.", SubmittedAt: s.IssueComments[0].CreatedAt.Add(time.Second)})
			case "wrong head":
				s.IssueComments[1].Body = strings.ReplaceAll(s.IssueComments[1].Body, intent.HeadSHA, strings.Repeat("e", 40))
			}
			got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
			if err != nil || got.Complete {
				t.Fatal("separate zero status bypassed original attribution guards", got, err)
			}
		})
	}
}

func TestHostedStandingStatusAcceptsOnlyLeadingPresentationBreaks(t *testing.T) {
	s, baseline, intent, poster, bot, earliest := annotationBaselineFixture(t)
	body := standingStatusBody(standingHeadingBody(decoratedStandingBody(s.IssueComments[1].Body, true)))
	body = strings.ReplaceAll(body, "<dl>\n<dd>\n", "<dl>\n<dd>\n<br>\n")
	s.IssueComments[1].Body = body
	got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
	if err != nil || !got.Complete || got.Publication.StandingZero == nil || len(got.Findings) != 0 {
		t.Fatal("leading presentation breaks lost the attributed inventory", got.Reason, err)
	}
	for name, malformed := range map[string]string{
		"repeated metadata break": strings.Replace(body, "<br>\n", "<br>\n<br>\n", 1),
		"repeated coverage break": strings.Replace(body, "<br>\n<!-- final_review_risk_start -->", "<br>\n<br>\n<!-- final_review_risk_start -->", 1),
		"break in file list":      strings.Replace(body, "* `pkg/alpha.go`", "<br>\n* `pkg/alpha.go`", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if malformed == body {
				t.Fatal("fixture mutation did not apply")
			}
			if _, ok := parseStandingZero(malformed); ok {
				t.Fatal("misplaced presentation break supplied zero inventory")
			}
		})
	}
}
