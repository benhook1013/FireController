package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func definitionListFinishedAck() string {
	return "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\n<dl>\n<dd>\n\nFull review finished.\n\n</dd>\n</dl>\n</details>"
}

func TestHostedFinishedAcknowledgementDefinitionListInventoryBoundary(t *testing.T) {
	for _, multiline := range []bool{false, true} {
		body := definitionListFinishedAck()
		if multiline {
			body = strings.Replace(body, "<summary>✅ Action performed</summary>", "<summary>\n✅ Action performed\n</summary>", 1)
		}
		for _, crlf := range []bool{false, true} {
			if crlf {
				body = strings.ReplaceAll(body, "\n", "\r\n")
			}
			for _, inventory := range []string{"complete", "unsupported", "missing"} {
				s, intent, poster, bot, earliest := standingTestFixture(t)
				s.IssueComments[2].Body = body
				if inventory == "unsupported" {
					s.IssueComments[1].Body = "Unsupported standing inventory"
				} else if inventory == "missing" {
					s.IssueComments = append(s.IssueComments[:1], s.IssueComments[2])
				}
				before := append([]IssueComment(nil), s.IssueComments...)
				got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
				if err != nil || !reflect.DeepEqual(before, s.IssueComments) {
					t.Fatal("acknowledgement changed source or failed extraction", err)
				}
				if inventory == "complete" {
					if !got.Complete || got.Publication.StandingZero == nil || got.Publication.StandingZero.ProviderRunID != standingTestRun || len(got.Findings) != 0 {
						t.Fatal("complete standing inventory lost its exact finished reply", got)
					}
				} else if got.Complete || got.Reason != "hosted_finished_inventory_unproven" || got.Publication.StandingZero != nil || !got.Publication.FinishedAt.IsZero() || len(got.Findings) != 0 {
					t.Fatal("acknowledgement manufactured empty-result credit", got)
				}
			}
		}
	}
}

func TestHostedFinishedAcknowledgementDefinitionListRejectsOtherMarkup(t *testing.T) {
	body := definitionListFinishedAck()
	for name, malformed := range map[string]string{
		"extra text":         body + "\nAdditional outcome",
		"unknown header":     strings.Replace(body, "generated:v1", "generated:v2", 1),
		"missing invocation": strings.Replace(body, "<!-- CodeRabbit review command invocation: v2:"+strings.Repeat("c", 64)+" -->", "", 1),
		"wrong label":        strings.Replace(body, "✅ Action performed", "Review pending", 1),
		"unknown container":  strings.ReplaceAll(strings.ReplaceAll(body, "<dl>", "<div>"), "</dl>", "</div>"),
		"repeated wrapper":   strings.Replace(body, "<dd>", "<dl>\n<dd>", 1),
		"broken nesting":     strings.Replace(body, "</dd>\n</dl>", "</dl>\n</dd>", 1),
		"quoted":             "> " + strings.ReplaceAll(body, "\n", "\n> "),
		"fenced":             "```html\n" + body + "\n```",
	} {
		t.Run(name, func(t *testing.T) {
			if standingFinishedAck(malformed) {
				t.Fatal("unsupported acknowledgement became proof")
			}
		})
	}
}
