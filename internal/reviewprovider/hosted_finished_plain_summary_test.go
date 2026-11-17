package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func plainFinishedSummaryAck() string {
	return strings.Replace(definitionListFinishedAck(), "✅ Action performed", "Action performed", 1)
}

func TestHostedFinishedPlainSummaryClosedEnvelopes(t *testing.T) {
	plain := plainFinishedSummaryAck()
	for name, body := range map[string]string{
		"one line definition list": plain,
		"three line CRLF":          strings.ReplaceAll(strings.Replace(plain, "<summary>Action performed</summary>", "<summary>\nAction performed\n</summary>", 1), "\n", "\r\n"),
		"one line direct":          strings.Replace(standingTestAck(), "✅ Action performed", "Action performed", 1),
		"decorated unchanged":      definitionListFinishedAck(),
	} {
		t.Run(name, func(t *testing.T) {
			if !standingFinishedAck(body) {
				t.Fatal("exact finished summary not recognized")
			}
		})
	}
}

func TestHostedFinishedPlainSummaryInventoryBoundary(t *testing.T) {
	for _, mode := range []string{"complete", "missing", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.IssueComments[2].Body = plainFinishedSummaryAck()
			if mode == "unsupported" {
				s.IssueComments[1].Body = "Unsupported standing inventory"
			} else if mode == "missing" {
				s.IssueComments = []IssueComment{s.IssueComments[0], s.IssueComments[2]}
			}
			before := append([]IssueComment(nil), s.IssueComments...)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatal("failed extraction or changed raw source", err)
			}
			if mode == "complete" {
				z := got.Publication.StandingZero
				if !got.Complete || got.Publication.State != "attributed" || z == nil || z.ProviderRunID != standingTestRun || z.Acknowledgement != before[2] || z.Summary != before[1] || z.HeadSHA != intent.HeadSHA || len(got.Findings) != 0 || !validStandingZero(z) {
					t.Fatal("complete inventory or original identity lost", got)
				}
			} else if got.Complete || got.Publication.State != "uncertain" || got.Reason != "hosted_finished_inventory_unproven" || got.Publication.StandingZero != nil || !got.Publication.FinishedAt.IsZero() || len(got.Findings) != 0 {
				t.Fatal("finished reply manufactured inventory or credit", got)
			}
		})
	}
}

func TestHostedFinishedPlainSummaryRejectsOtherGrammar(t *testing.T) {
	body := plainFinishedSummaryAck()
	for name, malformed := range map[string]string{
		"unknown decoration": strings.Replace(body, "Action performed", "✔ Action performed", 1),
		"arbitrary HTML":     strings.Replace(body, "Action performed", "<strong>Action performed</strong>", 1),
		"other case":         strings.Replace(body, "Action performed", "Action Performed", 1),
		"other outcome":      strings.Replace(body, "Full review finished.", "Review rate limited.", 1),
		"missing invocation": strings.Replace(body, "<!-- CodeRabbit review command invocation: v2:"+strings.Repeat("c", 64)+" -->", "", 1),
		"unknown header":     strings.Replace(body, "generated:v1", "generated:v2", 1),
		"repeated summary":   strings.Replace(body, "</summary>", "</summary>\n<summary>Action performed</summary>", 1),
		"unknown container":  strings.ReplaceAll(strings.ReplaceAll(body, "<dl>", "<div>"), "</dl>", "</div>"),
		"fenced":             "```html\n" + body + "\n```",
		"trailer":            body + "\nA second outcome.",
	} {
		t.Run(name, func(t *testing.T) {
			if standingFinishedAck(malformed) {
				t.Fatal("unsupported finished envelope accepted")
			}
		})
	}
	// The plain success label is not an alias for other command outcomes.
	limited := strings.Replace(body, "Full review finished.", "Review rate limited.", 1)
	if standingCommandReply(limited, "⚠️ Action not completed", "Review rate limited.") {
		t.Fatal("plain success label broadened rate-limit grammar")
	}
}

func TestHostedFinishedPlainSummaryAttributionGuards(t *testing.T) {
	for _, mode := range []string{"wrong actor", "at trigger", "competing command"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.IssueComments[1].Body = "Unsupported standing inventory"
			s.IssueComments[2].Body = plainFinishedSummaryAck()
			trigger := s.IssueComments[0].CreatedAt
			switch mode {
			case "wrong actor":
				s.IssueComments[2].Author = poster
			case "at trigger":
				s.IssueComments[2].CreatedAt = trigger
			case "competing command":
				at := trigger.Add(2 * time.Second)
				s.IssueComments = append(s.IssueComments, IssueComment{ID: 901, Author: poster, Body: "@coderabbitai full review", CreatedAt: at, UpdatedAt: at})
			}
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Reason == "hosted_finished_inventory_unproven" || !got.Publication.FinishedAt.IsZero() {
				t.Fatal("unsafe finished reply gained diagnostic or completion", got, err)
			}
		})
	}
}
