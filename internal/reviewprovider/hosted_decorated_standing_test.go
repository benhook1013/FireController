package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
)

func standingTestPicture(dark bool) string {
	source := ""
	if dark {
		source = `<source media="(prefers-color-scheme: dark)" srcset="https://assets.invalid/icon-dark.svg">`
	}
	return `<picture>` + source + `<img src="https://assets.invalid/icon.svg" width="16" height="16" align="absmiddle" alt=""></picture>`
}

func decoratedStandingBody(body string, dark bool) string {
	icon := standingTestPicture(dark)
	for _, label := range []string{"ℹ️ Recent review info", "⚙️ Run configuration", "📥 Commits", "⛔ Files ignored due to path filters:", "📒 Files selected for processing:", "💤 Files with no reviewable changes:"} {
		_, text, _ := strings.Cut(label, " ")
		body = strings.ReplaceAll(body, label, icon+" "+text)
	}
	body = strings.Replace(body, "🔵 Low", icon+" Minimal", 1)
	return strings.Replace(body, "No actionable comments were generated in the recent review. 🎉", "No actionable comments were generated in the recent review.", 1)
}

func TestHostedDecoratedStandingInventory(t *testing.T) {
	for _, dark := range []bool{false, true} {
		s, b, intent, poster, bot, earliest := annotationBaselineFixture(t)
		s.IssueComments[1].Body = decoratedStandingBody(s.IssueComments[1].Body, dark)
		s.IssueComments[2].Body = definitionListFinishedAck()
		s.ReviewComments[0].Body = b.ReviewComments[0].Body + "\n\n✅ Addressed in commits 0123456 to " + intent.HeadSHA[:7]
		before, _ := json.Marshal(s)
		baselineBefore, _ := json.Marshal(b)
		got, err := ExtractHostedFindingsWithBaseline(s, b, intent, poster, bot, earliest)
		if err != nil || !got.Complete || got.Publication.StandingZero == nil || got.Publication.StandingZero.SelectedFiles != 2 || got.Publication.StandingZero.IgnoredFiles != 1 || len(got.Findings) != 0 {
			t.Fatal("decorative metadata prevented complete attributed inventory", got, err)
		}
		after, _ := json.Marshal(s)
		baselineAfter, _ := json.Marshal(b)
		if string(before) != string(after) || string(baselineBefore) != string(baselineAfter) {
			t.Fatal("parser changed immutable source")
		}
	}
}

func TestHostedDecoratedStandingRejectsUnknownOrContradictoryProof(t *testing.T) {
	body := decoratedStandingBody(standingTestBody(strings.Repeat("c", 40), strings.Repeat("b", 40)), true)
	for name, mutation := range map[string]func(string) string{
		"http icon": func(s string) string {
			return strings.ReplaceAll(s, "https://assets.invalid/", "http://assets.invalid/")
		},
		"unknown attribute": func(s string) string { return strings.ReplaceAll(s, `alt=""`, `alt="" title="extra"`) },
		"source query": func(s string) string {
			return strings.ReplaceAll(s, `media="(prefers-color-scheme: dark)"`, `media="other"`)
		},
		"source duplicate": func(s string) string {
			return strings.ReplaceAll(s, `<source media=`, `<source media="(prefers-color-scheme: dark)" srcset="https://assets.invalid/a.svg"><source media=`)
		},
		"broken picture": func(s string) string { return strings.ReplaceAll(s, "</picture>", "") },
		"wrong label":    func(s string) string { return strings.ReplaceAll(s, "Recent review info", "Example review info") },
		"unknown risk":   func(s string) string { return strings.ReplaceAll(s, " Minimal</strong>", " Unclassified</strong>") },
		"missing coverage": func(s string) string {
			return strings.ReplaceAll(s, "final_review_risk_coverage:", "missing_coverage:")
		},
		"count mismatch": func(s string) string { return strings.ReplaceAll(s, "processing: 2", "processing: 3") },
		"contradictory zero": func(s string) string {
			return strings.ReplaceAll(s, "No actionable comments were generated in the recent review.", "No actionable comments were generated in the recent review. One finding remains.")
		},
		"unknown section": func(s string) string {
			return strings.Replace(s, "- **Run ID**:", "New unparsed section\n- **Run ID**:", 1)
		},
		"quoted": func(s string) string { return "> " + strings.ReplaceAll(s, "\n", "\n> ") },
		"fenced": func(s string) string { return "```html\n" + s + "\n```" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseStandingZero(mutation(body)); ok {
				t.Fatal("unknown source became zero proof")
			}
		})
	}
}

func TestHostedAnnotationRangeRequiresExactCandidateAndUnchangedRoot(t *testing.T) {
	for _, suffix := range []string{
		"\n\n✅ Addressed in commits 0123456 to " + strings.Repeat("d", 7),
		"\n\n✅ Addressed in commits 012345 to " + strings.Repeat("c", 7),
		"\n\n✅ Addressed in commits 0123456 to " + strings.Repeat("c", 6),
		"\n\n✅ Addressed in commits ABCDEF0 to " + strings.Repeat("c", 7),
		"\n\n✅ Addressed in commits 0123456 to " + strings.Repeat("c", 7) + "\nAnother change",
		"\n\n✅ Addressed in commits 0123456 through " + strings.Repeat("c", 7),
	} {
		s, b, intent, poster, bot, earliest := annotationBaselineFixture(t)
		s.ReviewComments[0].Body = b.ReviewComments[0].Body + suffix
		got, err := ExtractHostedFindingsWithBaseline(s, b, intent, poster, bot, earliest)
		if err != nil || got.Complete {
			t.Fatal("unknown range footer became proof", got, err)
		}
	}
	for _, change := range []string{"body", "location", "review", "after finish", "missing baseline", "missing ACK"} {
		t.Run(change, func(t *testing.T) {
			s, b, intent, poster, bot, earliest := annotationBaselineFixture(t)
			s.ReviewComments[0].Body = b.ReviewComments[0].Body + "\n\n✅ Addressed in commits 0123456 to " + intent.HeadSHA[:7]
			switch change {
			case "body":
				s.ReviewComments[0].Body = "Changed original\n" + s.ReviewComments[0].Body
			case "location":
				s.ReviewComments[0].Path = "elsewhere.go"
			case "review":
				s.Reviews[0].Body += "changed"
			case "after finish":
				s.ReviewComments[0].UpdatedAt = s.ObservedAt
			case "missing baseline":
				b.ReviewComments = nil
			case "missing ACK":
				s.IssueComments = s.IssueComments[:2]
			}
			got, err := ExtractHostedFindingsWithBaseline(s, b, intent, poster, bot, earliest)
			if err != nil || got.Complete {
				t.Fatal("range annotation weakened baseline/window proof", got, err)
			}
		})
	}
}
