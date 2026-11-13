package reviewprovider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Synthetic bodies use the observed closed progress envelope, not private
// provider captures. Old completed/paused sections may coexist outside it.
func processingTestBody(head string) string {
	return hostedProcessingStart + `

> [!NOTE]
>
>
> Currently processing new changes in this PR. This may take a few minutes, please wait...

<details>
<summary><strong>ℹ️ Review info</strong></summary>
<dl>
<dd>
<br>
<details>
<summary><strong>⚙️ Run configuration</strong></summary>
<dl>
<dd>
<br>
- **Configuration used**: Repository: synthetic/project/.coderabbit.yaml
- **Review profile**: ASSERTIVE
- **Plan**: Advanced
- **Run ID**: ` + "`" + standingTestRun + "`" + `
</dd>
</dl>
</details>
<details>
<summary><strong>📥 Commits</strong></summary>
<dl>
<dd>
<br>
Reviewing files that changed from the base of the PR and between ` + strings.Repeat("b", 40) + ` and ` + head + `.
</dd>
</dl>
</details>
<details>
<summary><strong>📒 Files selected for processing: 1</strong></summary>
<dl>
<dd>
<br>
* ` + "`source.go`" + `
</dd>
</dl>
</details>
</dd>
</dl>
</details>
` + hostedProcessingEnd
}

func processingFixture(t *testing.T) (HostedPublicationSnapshot, HostedPublicationSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	intent, poster, earliest, observed, trigger := triggerFixture(t)
	bot := Author{ID: 999, Login: "synthetic-review[bot]", Kind: "Bot"}
	old := IssueComment{ID: 601, Author: bot, CreatedAt: earliest.Add(-time.Hour), UpdatedAt: earliest.Add(-time.Minute), Body: "Older provider summary."}
	baseline := HostedPublicationSnapshot{Complete: true, ObservedAt: earliest, IssueComments: []IssueComment{old}, Reviews: []HostedReview{}, ReviewComments: []ReviewComment{}}
	current := old
	current.UpdatedAt, current.Body = trigger.CreatedAt.Add(3*time.Second), processingTestBody(intent.HeadSHA)
	snapshot := HostedPublicationSnapshot{Complete: true, ObservedAt: observed, IssueComments: []IssueComment{trigger, current}, Reviews: []HostedReview{}, ReviewComments: []ReviewComment{}}
	return snapshot, baseline, intent, poster, bot, earliest
}

func TestHostedProcessingObservationRetainsExactSourceAndDoesNotGrantCompletion(t *testing.T) {
	for _, currentFormat := range []bool{false, true} {
		s, baseline, intent, poster, bot, earliest := processingFixture(t)
		if currentFormat {
			body := strings.Replace(s.IssueComments[1].Body, "- **Configuration used**: Repository: synthetic/project/.coderabbit.yaml\n", strings.NewReplacer("CONFIG_HEAD", intent.HeadSHA, "CONFIG_SHORT", intent.HeadSHA[:7]).Replace(standingExpandedConfig), 1)
			icon := `<picture><source media="(prefers-color-scheme: dark)" srcset="https://example.test/dark.svg"><img src="https://example.test/icon.svg" width="16" height="16" align="absmiddle" alt=""></picture>`
			for _, emoji := range []string{"ℹ️", "⚙️", "📥", "📒"} {
				body = strings.ReplaceAll(body, emoji, icon)
			}
			s.IssueComments[1].Body = "<!-- coderabbit-ai-generated:v1 -->\n" + strings.ReplaceAll(body, "\n", "\r\n") + "\n<details><summary>Older result</summary>Previously finished review.</details>"
		}
		before, _ := json.Marshal([]HostedPublicationSnapshot{s, baseline})
		got, err := ExtractHostedProcessing(s, baseline, intent, poster, bot, earliest)
		if err != nil || got == nil || got.SourceCommentID != 601 || got.ProviderRunID != standingTestRun || got.TriggerID != "91" || got.ObservedAt != s.ObservedAt || got.CommentUpdatedAt != s.IssueComments[1].UpdatedAt || got.ObservedAt == s.IssueComments[1].CreatedAt {
			t.Fatalf("lost matching processing observation (current=%v): %+v %v", currentFormat, got, err)
		}
		again, err := ExtractHostedProcessing(s, baseline, intent, poster, bot, earliest)
		if err != nil || !reflect.DeepEqual(got, again) {
			t.Fatal("same retained capture did not give the same signal")
		}
		publication, err := ResolveHostedPublication(s, intent, poster, bot, earliest)
		if err != nil || publication.State != "active" || publication.Reason != "publication_not_proven" || !publication.FinishedAt.IsZero() {
			t.Fatal("processing supplied completion credit", publication, err)
		}
		after, _ := json.Marshal([]HostedPublicationSnapshot{s, baseline})
		if string(before) != string(after) {
			t.Fatal("parser mutated retained evidence")
		}
	}
}

func TestHostedProcessingRejectsUnattributableOrOldSignals(t *testing.T) {
	changes := map[string]func(*HostedPublicationSnapshot, *HostedPublicationSnapshot){
		"wrong head": func(s, _ *HostedPublicationSnapshot) {
			s.IssueComments[1].Body = processingTestBody(strings.Repeat("e", 40))
		},
		"spoofed actor":         func(s, _ *HostedPublicationSnapshot) { s.IssueComments[1].Author.ID++ },
		"login is not identity": func(s, _ *HostedPublicationSnapshot) { s.IssueComments[1].Author.Kind = "User" },
		"missing trigger":       func(s, _ *HostedPublicationSnapshot) { s.IssueComments = s.IssueComments[1:] },
		"edited trigger": func(s, _ *HostedPublicationSnapshot) {
			s.IssueComments[0].UpdatedAt = s.IssueComments[0].UpdatedAt.Add(time.Second)
		},
		"future comment": func(s, _ *HostedPublicationSnapshot) { s.IssueComments[1].UpdatedAt = s.ObservedAt.Add(time.Second) },
		"before trigger": func(s, _ *HostedPublicationSnapshot) {
			s.IssueComments[1].UpdatedAt = s.IssueComments[0].CreatedAt.Add(-time.Second)
		},
		"incomplete current":    func(s, _ *HostedPublicationSnapshot) { s.Complete = false },
		"incomplete baseline":   func(_, b *HostedPublicationSnapshot) { b.Complete = false },
		"missing baseline time": func(_, b *HostedPublicationSnapshot) { b.ObservedAt = time.Time{} },
		"late baseline":         func(s, b *HostedPublicationSnapshot) { b.ObservedAt = s.ObservedAt },
		"changed comment identity": func(_, b *HostedPublicationSnapshot) {
			b.IssueComments[0].CreatedAt = b.IssueComments[0].CreatedAt.Add(-time.Second)
		},
		"unknown old comment": func(_, b *HostedPublicationSnapshot) { b.IssueComments = []IssueComment{} },
		"old run": func(_, b *HostedPublicationSnapshot) {
			b.IssueComments[0].Body = "An older result with run " + standingTestRun
		},
		"same run in baseline review": func(s, b *HostedPublicationSnapshot) {
			b.Reviews = []HostedReview{{ID: 99, Author: s.IssueComments[1].Author, State: "commented", HeadSHA: strings.Repeat("c", 40), SubmittedAt: b.ObservedAt.Add(-time.Second), Body: standingTestRun}}
		},
		"competing progress comments": func(s, _ *HostedPublicationSnapshot) {
			c := s.IssueComments[1]
			c.ID++
			c.CreatedAt = c.UpdatedAt
			s.IssueComments = append(s.IssueComments, c)
		},
		"later command closes attribution": func(s, _ *HostedPublicationSnapshot) {
			c := s.IssueComments[0]
			c.ID++
			c.Body = "@coderabbitai review"
			c.CreatedAt = c.CreatedAt.Add(time.Second)
			c.UpdatedAt = c.CreatedAt
			s.IssueComments = append(s.IssueComments, c)
		},
		"finished acknowledgment supersedes progress": func(s, _ *HostedPublicationSnapshot) {
			c := s.IssueComments[1]
			c.ID++
			c.CreatedAt = c.UpdatedAt
			c.Body = "<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("a", 64) + " -->\n<details>\n<summary>Action performed</summary>\nFull review finished.\n</details>"
			s.IssueComments = append(s.IssueComments, c)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, b, intent, poster, bot, earliest := processingFixture(t)
			change(&s, &b)
			got, err := ExtractHostedProcessing(s, b, intent, poster, bot, earliest)
			if err != nil || got != nil {
				t.Fatal("unattributed processing became a start signal", got, err)
			}
		})
	}
}

func TestHostedProcessingRejectsHiddenMalformedOrConflictingBlocks(t *testing.T) {
	changes := map[string]func(string) string{
		"quoted":         func(b string) string { return "> " + strings.ReplaceAll(b, "\n", "\n> ") },
		"fenced":         func(b string) string { return "```html\n" + b + "\n```" },
		"indented":       func(b string) string { return "    " + strings.ReplaceAll(b, "\n", "\n    ") },
		"hidden":         func(b string) string { return "<!--\n" + b + "\n-->" },
		"nested":         func(b string) string { return "<details>\n<summary>Example</summary>\n" + b + "\n</details>" },
		"duplicate":      func(b string) string { return b + "\n" + b },
		"missing end":    func(b string) string { return strings.Replace(b, hostedProcessingEnd, "", 1) },
		"changed notice": func(b string) string { return strings.Replace(b, "Currently processing", "Not processing", 1) },
		"global head only": func(b string) string {
			return strings.Replace(b, strings.Repeat("c", 40), strings.Repeat("e", 40), 1) + "\nCurrent head " + strings.Repeat("c", 40)
		},
		"global run only": func(b string) string {
			return strings.Replace(b, "- **Run ID**: `"+standingTestRun+"`", "", 1) + "\n- **Run ID**: `" + standingTestRun + "`"
		},
		"duplicate run": func(b string) string {
			return strings.Replace(b, "- **Run ID**: ", "- **Run ID**: `"+standingTestRun+"`\n- **Run ID**: ", 1)
		},
		"missing close": func(b string) string { return strings.Replace(b, "</dd>", "", 1) },
		"unknown configuration": func(b string) string {
			return strings.Replace(b, "- **Plan**: Advanced", "- **Unrecognized**: Advanced", 1)
		},
		"inventory mismatch": func(b string) string { return strings.Replace(b, "processing: 1", "processing: 2", 1) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, b, intent, poster, bot, earliest := processingFixture(t)
			s.IssueComments[1].Body = change(s.IssueComments[1].Body)
			got, err := ExtractHostedProcessing(s, b, intent, poster, bot, earliest)
			if err != nil || got != nil {
				t.Fatal("unsupported progress body supplied a signal", got, err)
			}
		})
	}
}

func TestHostedProcessingAllowsFreshProviderCommentButNotTriggerReplyAlone(t *testing.T) {
	s, b, intent, poster, bot, earliest := processingFixture(t)
	b.IssueComments = []IssueComment{}
	s.IssueComments[1].CreatedAt = s.IssueComments[1].UpdatedAt
	got, err := ExtractHostedProcessing(s, b, intent, poster, bot, earliest)
	if err != nil || got == nil {
		t.Fatal("fresh matching provider progress was lost", err)
	}
	s.IssueComments[1].Body = "<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("a", 64) + " -->\n<details>\n<summary>Action performed</summary>\nFull review triggered.\n</details>"
	got, err = ExtractHostedProcessing(s, b, intent, poster, bot, earliest)
	if err != nil || got != nil {
		t.Fatal("trigger acknowledgment alone claimed processing", got, err)
	}
}

func TestHostedProcessingAcceptsBoundPrePostBaselineWithinGitHubTriggerSecond(t *testing.T) {
	s, b, intent, poster, bot, earliest := processingFixture(t)
	b.ObservedAt = s.IssueComments[0].CreatedAt.Add(250 * time.Millisecond)
	got, err := ExtractHostedProcessing(s, b, intent, poster, bot, earliest)
	if err != nil || got == nil {
		t.Fatal("second-resolution provider timestamp rejected exact runtime-bound baseline", got, err)
	}
	b.ObservedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
	got, err = ExtractHostedProcessing(s, b, intent, poster, bot, earliest)
	if err != nil || got != nil {
		t.Fatal("next-second baseline accepted", got, err)
	}
}
