package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func TestHostedStandingReviewInfoContentMarker(t *testing.T) {
	anchor := "<summary><strong>ℹ️ Recent review info</strong></summary>\n<dl>\n<dd>\n"
	for _, crlf := range []bool{false, true} {
		s, intent, poster, bot, earliest := standingTestFixture(t)
		body := strings.Replace(s.IssueComments[1].Body, anchor, anchor+"<!-- review_info_content -->\n", 1)
		if crlf {
			body = strings.ReplaceAll(body, "\n", "\r\n")
		}
		s.IssueComments[1].Body = body
		before := append([]IssueComment(nil), s.IssueComments...)
		got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
		if err != nil || !got.Complete || len(got.Findings) != 0 || got.Publication.StandingZero == nil || got.Publication.StandingZero.ProviderRunID != standingTestRun || !reflect.DeepEqual(s.IssueComments, before) {
			t.Fatal("presentation marker changed or lost proven result", got.Reason, err)
		}
	}
	for name, edit := range map[string]func(string) string{
		"repeated": func(b string) string {
			return strings.Replace(b, anchor, anchor+"<!-- review_info_content -->\n<!-- review_info_content -->\n", 1)
		},
		"unknown": func(b string) string {
			return strings.Replace(b, anchor, anchor+"<!-- review_info_content_v2 -->\n", 1)
		},
		"before heading": func(b string) string { return strings.Replace(b, anchor, "<!-- review_info_content -->\n"+anchor, 1) },
		"inside configuration": func(b string) string {
			return strings.Replace(b, "- **Run ID**:", "<!-- review_info_content -->\n- **Run ID**:", 1)
		},
		"quoted": func(b string) string { return strings.Replace(b, anchor, anchor+"> <!-- review_info_content -->\n", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.IssueComments[1].Body = edit(s.IssueComments[1].Body)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Reason != "hosted_finished_inventory_unproven" {
				t.Fatal("unknown markup credited or finished reply hidden", got.Reason, err)
			}
		})
	}
}

func TestHostedBaselineBotRefreshPreservesFinishedDiagnostic(t *testing.T) {
	s, intent, poster, bot, earliest := standingTestFixture(t)
	s.IssueComments[1].Body = "Unsupported retained result."
	s, baseline := botRefreshFixture(s)
	got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
	if err != nil || got.Complete || got.Reason != "hosted_finished_inventory_unproven" || got.Publication.State != "uncertain" || !got.Publication.FinishedAt.IsZero() || len(got.Findings) != 0 {
		t.Fatal("finished diagnostic hidden or credited", got.Reason, err)
	}
	baseline.IssueComments[0].Author.Kind = "User"
	again, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
	if err != nil || again.Complete || again.Reason != "attribution_window_closed" {
		t.Fatal("unproven bot baseline relaxed attribution", again.Reason, err)
	}
}
