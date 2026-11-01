package reviewprovider

import (
	"reflect"
	"testing"
	"time"
)

func allowanceBaselineFixture(t *testing.T) (HostedAllowanceSnapshot, HostedAllowanceSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	current, intent, poster, provider, earliest := allowanceFixture(t)
	query := current.IssueComments[0]
	report := IssueComment{ID: 94, Body: "Automated build report, revision two.", Author: Author{ID: 303, Login: "synthetic-build-bot", Kind: "Bot"}, CreatedAt: query.CreatedAt.Add(-time.Hour), UpdatedAt: query.CreatedAt.Add(2 * time.Second)}
	current.IssueComments = append(current.IssueComments, report)
	baseline := HostedAllowanceSnapshot{Complete: true, ObservedAt: query.CreatedAt.Add(time.Second), IssueComments: []IssueComment{query, report}}
	baseline.IssueComments[1].Body = "Automated build report, revision one."
	baseline.IssueComments[1].UpdatedAt = query.CreatedAt.Add(-time.Minute)
	return current, baseline, intent, poster, provider, earliest
}

func TestHostedAllowanceWithBaselineProvesOnlyUnrelatedBotRefresh(t *testing.T) {
	for _, state := range []string{"pending", "available", "cooldown"} {
		t.Run(state, func(t *testing.T) {
			current, baseline, intent, poster, provider, earliest := allowanceBaselineFixture(t)
			if state == "pending" {
				current.IssueComments = append(current.IssueComments[:1:1], current.IssueComments[2:]...)
			}
			if state == "available" {
				current.IssueComments[1].Body = hostedAllowanceAvailable
			}
			strict := resolveAllowance(t, current, intent, poster, provider, earliest)
			if strict.Reason != "query_window_closed" {
				t.Fatal("fixture failed to exercise strict hold", strict)
			}
			before, oldBefore := append([]IssueComment(nil), current.IssueComments...), append([]IssueComment(nil), baseline.IssueComments...)
			got, err := ResolveHostedAllowanceWithBaseline(current, baseline, intent, poster, provider, earliest)
			want := "allowance_not_proven"
			if state != "pending" {
				want = "provider_" + state + "_observed"
			}
			if err != nil || got.Reason != want || got.QueryCommentID != strict.QueryCommentID || got.QueryCreatedAt != strict.QueryCreatedAt {
				t.Fatalf("baseline did not retain attribution: %+v %v", got, err)
			}
			if state != "pending" && (got.ResponseCommentID != 92 || got.ResponseCreatedAt != current.IssueComments[1].CreatedAt) {
				t.Fatal("reply identity changed", got)
			}
			if state == "cooldown" && got.RetryNotBefore != current.IssueComments[1].CreatedAt.Add(38*time.Minute) {
				t.Fatal("rounding guard lost", got)
			}
			if !reflect.DeepEqual(current.IssueComments, before) || !reflect.DeepEqual(baseline.IssueComments, oldBefore) {
				t.Fatal("resolver changed immutable inputs")
			}
		})
	}
}

func TestHostedAllowanceWithBaselineRejectsUnprovenEdits(t *testing.T) {
	for name, change := range map[string]func(*HostedAllowanceSnapshot, *HostedAllowanceSnapshot){
		"missing bot":         func(c, b *HostedAllowanceSnapshot) { b.IssueComments = b.IssueComments[:1] },
		"different id":        func(c, b *HostedAllowanceSnapshot) { b.IssueComments[1].ID++ },
		"different author id": func(c, b *HostedAllowanceSnapshot) { b.IssueComments[1].Author.ID++ },
		"different login":     func(c, b *HostedAllowanceSnapshot) { b.IssueComments[1].Author.Login = "other-bot" },
		"human": func(c, b *HostedAllowanceSnapshot) {
			c.IssueComments[2].Author.Kind = "User"
			b.IssueComments[1].Author.Kind = "User"
		},
		"poster bot": func(c, b *HostedAllowanceSnapshot) {
			c.IssueComments[2].Author = c.IssueComments[0].Author
			b.IssueComments[1].Author = c.IssueComments[0].Author
		},
		"different creation": func(c, b *HostedAllowanceSnapshot) {
			b.IssueComments[1].CreatedAt = b.IssueComments[1].CreatedAt.Add(-time.Second)
		},
		"new bot": func(c, b *HostedAllowanceSnapshot) {
			c.IssueComments[2].CreatedAt = c.IssueComments[0].CreatedAt
			b.IssueComments[1].CreatedAt = c.IssueComments[0].CreatedAt
			b.IssueComments[1].UpdatedAt = c.IssueComments[0].CreatedAt
		},
		"old edit at query":          func(c, b *HostedAllowanceSnapshot) { b.IssueComments[1].UpdatedAt = c.IssueComments[0].CreatedAt },
		"old edit after query":       func(c, b *HostedAllowanceSnapshot) { b.IssueComments[1].UpdatedAt = b.ObservedAt },
		"baseline at edit":           func(c, b *HostedAllowanceSnapshot) { b.ObservedAt = c.IssueComments[2].UpdatedAt },
		"baseline after edit":        func(c, b *HostedAllowanceSnapshot) { b.ObservedAt = c.IssueComments[2].UpdatedAt.Add(time.Second) },
		"baseline after observation": func(c, b *HostedAllowanceSnapshot) { b.ObservedAt = c.ObservedAt.Add(time.Second) },
		"old time outside snapshot":  func(c, b *HostedAllowanceSnapshot) { b.ObservedAt = b.IssueComments[1].UpdatedAt.Add(-time.Second) },
		"baseline incomplete":        func(c, b *HostedAllowanceSnapshot) { b.Complete = false },
		"baseline duplicate":         func(c, b *HostedAllowanceSnapshot) { b.IssueComments = append(b.IssueComments, b.IssueComments[1]) },
		"baseline malformed":         func(c, b *HostedAllowanceSnapshot) { b.IssueComments[1].Body = "\x00" },
		"baseline conflicting query": func(c, b *HostedAllowanceSnapshot) {
			q := b.IssueComments[0]
			q.ID++
			b.IssueComments = append(b.IssueComments, q)
		},
		"different query id": func(c, b *HostedAllowanceSnapshot) { b.IssueComments[0].ID++ },
		"different query time": func(c, b *HostedAllowanceSnapshot) {
			b.IssueComments[0].CreatedAt = b.IssueComments[0].CreatedAt.Add(-time.Nanosecond)
			b.IssueComments[0].UpdatedAt = b.IssueComments[0].CreatedAt
		},
		"old hidden command":     func(c, b *HostedAllowanceSnapshot) { b.IssueComments[1].Body += "<!-- @CodeRabbitAI review -->" },
		"current hidden command": func(c, b *HostedAllowanceSnapshot) { c.IssueComments[2].Body += "<!-- @coderabbitai rate limit -->" },
		"old provider name":      func(c, b *HostedAllowanceSnapshot) { b.IssueComments[1].Body += " CodeRabbit" },
		"current provider name":  func(c, b *HostedAllowanceSnapshot) { c.IssueComments[2].Body += " coderabbit" },
		"unknown second bot": func(c, b *HostedAllowanceSnapshot) {
			r := c.IssueComments[2]
			r.ID++
			c.IssueComments = append(c.IssueComments, r)
		},
		"human edited": func(c, b *HostedAllowanceSnapshot) {
			r := c.IssueComments[2]
			r.ID++
			r.Author = c.IssueComments[0].Author
			c.IssueComments = append(c.IssueComments, r)
		},
		"competing command": func(c, b *HostedAllowanceSnapshot) {
			r := c.IssueComments[2]
			r.ID++
			r.Body = "@coderabbitai review"
			r.CreatedAt = c.IssueComments[0].CreatedAt.Add(time.Second)
			r.UpdatedAt = r.CreatedAt
			c.IssueComments = append(c.IssueComments, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			current, baseline, intent, poster, provider, earliest := allowanceBaselineFixture(t)
			change(&current, &baseline)
			strict := resolveAllowance(t, current, intent, poster, provider, earliest)
			got, err := ResolveHostedAllowanceWithBaseline(current, baseline, intent, poster, provider, earliest)
			if err != nil || got != strict {
				t.Fatalf("unsafe baseline changed strict result: %+v != %+v (%v)", got, strict, err)
			}
		})
	}
}

func TestHostedAllowanceWithBaselinePreservesStrictReplyFailures(t *testing.T) {
	for _, mode := range []string{"unknown", "edited", "duplicate", "incomplete", "malformed current"} {
		t.Run(mode, func(t *testing.T) {
			current, baseline, intent, poster, provider, earliest := allowanceBaselineFixture(t)
			switch mode {
			case "unknown":
				current.IssueComments[1].Body = "Unknown provider answer"
			case "edited":
				current.IssueComments[1].UpdatedAt = current.IssueComments[1].UpdatedAt.Add(time.Second)
			case "duplicate":
				r := current.IssueComments[1]
				r.ID++
				current.IssueComments = append(current.IssueComments, r)
			case "incomplete":
				current.Complete = false
			case "malformed current":
				current.IssueComments[2].Body = "\x00"
			}
			got, err := ResolveHostedAllowanceWithBaseline(current, baseline, intent, poster, provider, earliest)
			if err != nil || got.State == "available_observed" || got.State == "cooldown_observed" || got.ResponseCommentID != 0 || !got.RetryNotBefore.IsZero() {
				t.Fatalf("unsafe reply gained attribution: %+v %v", got, err)
			}
		})
	}
}

func TestHostedAllowanceWithBaselineCannotForgetEarlierConflict(t *testing.T) {
	for _, mode := range []string{"command", "unknown provider", "edited provider", "duplicate provider", "answered before command"} {
		t.Run(mode, func(t *testing.T) {
			current, baseline, intent, poster, provider, earliest := allowanceBaselineFixture(t)
			c := baseline.IssueComments[0]
			c.ID = 96
			c.CreatedAt = c.CreatedAt.Add(time.Nanosecond)
			c.UpdatedAt = c.CreatedAt
			c.Body = "@coderabbitai review"
			if mode != "command" && mode != "answered before command" {
				c.Author = provider
				c.Body = "Unknown provider answer"
			}
			if mode == "edited provider" {
				c.Body = hostedAllowanceAvailable
				c.UpdatedAt = c.UpdatedAt.Add(time.Nanosecond)
			}
			if mode == "duplicate provider" {
				c.Body = hostedAllowanceAvailable
				d := c
				d.ID++
				baseline.IssueComments = append(baseline.IssueComments, d)
			}
			if mode == "answered before command" {
				r := c
				r.ID = 98
				r.Author = provider
				r.Body = hostedAllowanceAvailable
				c.CreatedAt = c.CreatedAt.Add(time.Nanosecond)
				c.UpdatedAt = c.CreatedAt
				baseline.IssueComments = append(baseline.IssueComments, r)
			}
			baseline.IssueComments = append(baseline.IssueComments, c)
			got, err := ResolveHostedAllowanceWithBaseline(current, baseline, intent, poster, provider, earliest)
			strict := resolveAllowance(t, current, intent, poster, provider, earliest)
			if err != nil || got != strict {
				t.Fatal("earlier known conflict was forgotten", got, err)
			}
		})
	}
}

func TestHostedAllowanceWithBaselinePreservesStrictFirst(t *testing.T) {
	current, baseline, intent, poster, provider, earliest := allowanceBaselineFixture(t)
	current.IssueComments = current.IssueComments[:2]
	baseline.Complete = false
	got, err := ResolveHostedAllowanceWithBaseline(current, baseline, intent, poster, provider, earliest)
	strict := resolveAllowance(t, current, intent, poster, provider, earliest)
	if err != nil || got != strict || got.State != "cooldown_observed" {
		t.Fatal("strict positive changed by irrelevant baseline", got, err)
	}
	current.IssueComments = append(current.IssueComments, current.IssueComments[1])
	got, err = ResolveHostedAllowanceWithBaseline(current, baseline, intent, poster, provider, earliest)
	strict = resolveAllowance(t, current, intent, poster, provider, earliest)
	if err != nil || got != strict || got.State != "ambiguous" {
		t.Fatal("strict ambiguity changed by baseline", got, err)
	}
}
