package reviewprovider

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Independently authored synthetic identities surround the single observed
// provider format. No live review, account data or private response is embedded.
func allowanceReply(minutes int) string {
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	return "<!-- This is an auto-generated reply by CodeRabbit -->\nYour [plan](https://docs.coderabbit.ai/management/plans#fair-usage-limits-policy) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). More reviews will be available in " + fmt.Sprint(minutes) + " " + unit + "."
}

func allowanceFixture(t *testing.T) (HostedAllowanceSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	intent, poster, earliest, observed, query := triggerFixture(t)
	body, err := HostedAllowanceQueryBody(intent)
	if err != nil {
		t.Fatal(err)
	}
	query.Body = body
	bot := Author{ID: 202, Login: "synthetic-provider", Kind: "Bot"}
	responseAt := query.CreatedAt.Add(5 * time.Second)
	snapshot := HostedAllowanceSnapshot{Complete: true, ObservedAt: observed, IssueComments: []IssueComment{query, {ID: 92, Body: allowanceReply(37), Author: bot, CreatedAt: responseAt, UpdatedAt: responseAt}}}
	return snapshot, intent, poster, bot, earliest
}

func resolveAllowance(t *testing.T, s HostedAllowanceSnapshot, i HostedTriggerIntent, p, b Author, e time.Time) HostedAllowanceObservation {
	t.Helper()
	result, err := ResolveHostedAllowance(s, i, p, b, e)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHostedAllowanceExactQueryAndObservedCooldown(t *testing.T) {
	s, intent, poster, bot, earliest := allowanceFixture(t)
	want := "@coderabbitai rate limit\n\n<!-- firecontroller-allowance-query v1 operation=" + intent.OperationID + " attempt=" + intent.AttemptID + " head=" + intent.HeadSHA + " -->"
	if s.IssueComments[0].Body != want {
		t.Fatal("query body changed")
	}
	original := append([]IssueComment(nil), s.IssueComments...)
	for _, minutes := range []int{1, 37, 1439} {
		s.IssueComments[1].Body = allowanceReply(minutes)
		s.IssueComments[0].Author.Login = "renamed-poster"
		s.IssueComments[1].Author.Login = "renamed-provider"
		result := resolveAllowance(t, s, intent, poster, bot, earliest)
		if result.State != "cooldown_observed" || result.Reason != "provider_cooldown_observed" || result.QueryCommentID != 91 || result.ResponseCommentID != 92 || result.QueryCreatedAt != s.IssueComments[0].CreatedAt || result.ResponseCreatedAt != s.IssueComments[1].CreatedAt || result.RetryNotBefore != s.IssueComments[1].CreatedAt.Add(time.Duration(minutes+1)*time.Minute) {
			t.Fatalf("exact cooldown not conservatively retained: %+v", result)
		}
		if replay := resolveAllowance(t, s, intent, poster, bot, earliest); replay != result {
			t.Fatal("replay changed observation")
		}
		encoded, _ := json.Marshal(result)
		for _, private := range []string{s.IssueComments[1].Body, "includedReviews", "remaining", "billing", "37 minutes"} {
			if strings.Contains(string(encoded), private) {
				t.Fatal("safe result exposes raw source or inferred fields")
			}
		}
		result.RetryNotBefore = time.Time{}
		if resolveAllowance(t, s, intent, poster, bot, earliest).RetryNotBefore.IsZero() {
			t.Fatal("returned output aliases later result")
		}
	}
	// Resolver does not mutate source; test edits above are deliberate local ones.
	s.IssueComments = original
	before := append([]IssueComment(nil), s.IssueComments...)
	_ = resolveAllowance(t, s, intent, poster, bot, earliest)
	if !reflect.DeepEqual(before, s.IssueComments) {
		t.Fatal("source changed")
	}
}

func TestHostedAllowanceUnknownEvidenceNeverGrantsPositiveCapacity(t *testing.T) {
	for name, change := range map[string]func(*HostedAllowanceSnapshot){
		"missing query":             func(s *HostedAllowanceSnapshot) { s.IssueComments = s.IssueComments[1:] },
		"unmarked historical query": func(s *HostedAllowanceSnapshot) { s.IssueComments[0].Body = "@coderabbitai rate limit" },
		"missing reply":             func(s *HostedAllowanceSnapshot) { s.IssueComments = s.IssueComments[:1] },
		"incomplete history":        func(s *HostedAllowanceSnapshot) { s.Complete = false },
		"empty complete":            func(s *HostedAllowanceSnapshot) { s.IssueComments = nil },
		"positive":                  func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Body = "You have 3 reviews available." },
		"unknown prose":             func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Body = "More reviews may be available soon." },
		"edited reply": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].UpdatedAt = s.IssueComments[1].UpdatedAt.Add(time.Second)
		},
		"unknown alongside supported": func(s *HostedAllowanceSnapshot) {
			copy := s.IssueComments[1]
			copy.ID++
			copy.Body = "You have one review available."
			s.IssueComments = append(s.IssueComments, copy)
		},
		"future reply": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].CreatedAt = s.ObservedAt.Add(time.Second)
			s.IssueComments[1].UpdatedAt = s.IssueComments[1].CreatedAt
		},
		"same instant reply": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].CreatedAt = s.IssueComments[0].CreatedAt
			s.IssueComments[1].UpdatedAt = s.IssueComments[1].CreatedAt
		},
		"earlier reply": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].CreatedAt = s.IssueComments[0].CreatedAt.Add(-time.Second)
			s.IssueComments[1].UpdatedAt = s.IssueComments[1].CreatedAt
		},
		"foreign same login": func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Author.ID++ },
		"foreign kind":       func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Author.Kind = "User" },
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			change(&s)
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State == "cooldown_observed" || !got.RetryNotBefore.IsZero() || got.ResponseCommentID != 0 {
				t.Fatal("unproven evidence acquired cooldown/reset proof", got)
			}
		})
	}
}

func TestHostedAllowanceMalformedAndHiddenReplyGrammar(t *testing.T) {
	for name, body := range map[string]string{
		"no wrapper":         strings.TrimPrefix(allowanceReply(37), "<!-- This is an auto-generated reply by CodeRabbit -->\n"),
		"extra prefix":       "Example:\n" + allowanceReply(37),
		"extra suffix":       allowanceReply(37) + "\nAnother obligation.",
		"extra newline":      allowanceReply(37) + "\n",
		"extra paragraph":    strings.Replace(allowanceReply(37), "-->\n", "-->\n\n", 1),
		"wrong link":         strings.Replace(allowanceReply(37), "docs.coderabbit.ai", "foreign.invalid", 1),
		"zero":               allowanceReply(0),
		"negative":           allowanceReply(-1),
		"24h plus margin":    allowanceReply(1440),
		"large":              allowanceReply(99999999),
		"leading zero":       strings.Replace(allowanceReply(37), "37 minutes", "037 minutes", 1),
		"decimal":            strings.Replace(allowanceReply(37), "37 minutes", "37.5 minutes", 1),
		"hours":              strings.Replace(allowanceReply(37), "37 minutes", "1 hour", 1),
		"markdown quote":     "> " + strings.ReplaceAll(allowanceReply(37), "\n", "\n> "),
		"fenced":             "```text\n" + allowanceReply(37) + "\n```",
		"pre":                "<pre>\n" + allowanceReply(37) + "\n</pre>",
		"html comment":       "<!-- example\n" + allowanceReply(37) + "\n-->",
		"duplicate template": allowanceReply(37) + "\n" + allowanceReply(37),
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			s.IssueComments[1].Body = body
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State != "unproven" || !got.RetryNotBefore.IsZero() {
				t.Fatal("unrecognized source format became a cooldown fact", got)
			}
		})
	}
}

func TestHostedAllowanceExactQueryRecoveryRejectsConflictingCopies(t *testing.T) {
	for name, change := range map[string]func(*IssueComment){
		"edited":          func(c *IssueComment) { c.UpdatedAt = c.UpdatedAt.Add(time.Second) },
		"altered command": func(c *IssueComment) { c.Body = strings.Replace(c.Body, "rate limit", "reviews remaining?", 1) },
		"head": func(c *IssueComment) {
			c.Body = strings.Replace(c.Body, strings.Repeat("c", 40), strings.Repeat("d", 40), 1)
		},
		"foreign author":   func(c *IssueComment) { c.Author.ID++ },
		"quoted":           func(c *IssueComment) { c.Body = "> " + strings.ReplaceAll(c.Body, "\n", "\n> ") },
		"fenced":           func(c *IssueComment) { c.Body = "```\n" + c.Body + "\n```" },
		"malformed marker": func(c *IssueComment) { c.Body = strings.TrimSuffix(c.Body, " -->") },
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			copy := s.IssueComments[0]
			copy.ID = 93
			change(&copy)
			s.IssueComments = append(s.IssueComments, copy)
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State != "ambiguous" || got.QueryCommentID != 0 || !got.RetryNotBefore.IsZero() {
				t.Fatal("conflicting query copy silently ignored", got)
			}
		})
	}
	for _, sameID := range []bool{false, true} {
		t.Run(fmt.Sprint(sameID), func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			copy := s.IssueComments[1]
			if !sameID {
				copy.ID++
			}
			s.IssueComments = append(s.IssueComments, copy)
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State != "ambiguous" || !got.RetryNotBefore.IsZero() {
				t.Fatal("duplicate response chosen", got)
			}
		})
	}
}

func TestHostedAllowanceRemovedCompetingCommandsRemainUnproven(t *testing.T) {
	for _, body := range []string{"Command removed.", "```text\n@coderabbitai quota\n```", "> @coderabbitai quota"} {
		for _, older := range []bool{false, true} {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			competitor := s.IssueComments[0]
			competitor.ID, competitor.Body = 93, body
			competitor.Author.ID++
			competitor.CreatedAt = competitor.CreatedAt.Add(time.Second)
			if older {
				competitor.CreatedAt = s.IssueComments[0].CreatedAt.Add(-time.Second)
			}
			competitor.UpdatedAt = s.IssueComments[1].CreatedAt.Add(time.Second)
			s.IssueComments = append(s.IssueComments, competitor)
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State != "unproven" || got.Reason != "query_window_closed" || got.ResponseCommentID != 0 || !got.RetryNotBefore.IsZero() {
				t.Fatalf("edited competitor was attributed to our query: %+v", got)
			}
		}
	}
}

func TestHostedAllowanceCompetingVisibleCommandsCloseAttribution(t *testing.T) {
	for _, command := range []string{"@coderabbitai rate limit", "@coderabbitai review", "@coderabbitai full review", "@CODERABBITAI quota", "@coderabbitai resume"} {
		s, intent, poster, bot, earliest := allowanceFixture(t)
		competitor := s.IssueComments[0]
		competitor.ID = 93
		competitor.Body = command
		competitor.Author.ID++
		competitor.CreatedAt = competitor.CreatedAt.Add(time.Second)
		competitor.UpdatedAt = competitor.CreatedAt
		s.IssueComments = append(s.IssueComments, competitor)
		got := resolveAllowance(t, s, intent, poster, bot, earliest)
		if got.State != "unproven" || got.Reason != "query_window_closed" || !got.RetryNotBefore.IsZero() {
			t.Fatal("competing query/review did not close causal attribution", got)
		}
	}
	for name, wrap := range map[string]func(string) string{
		"quote":      func(body string) string { return "> " + body },
		"fence":      func(body string) string { return "```text\n" + body + "\n```" },
		"indented":   func(body string) string { return "    " + body },
		"pre":        func(body string) string { return "<pre>\n" + body + "\n</pre>" },
		"blockquote": func(body string) string { return "<blockquote>\n" + body + "\n</blockquote>" },
		"comment":    func(body string) string { return "<!-- example\n" + body + "\n-->" },
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			example := s.IssueComments[0]
			example.ID = 93
			example.Body = wrap("@coderabbitai rate limit")
			example.CreatedAt = example.CreatedAt.Add(time.Second)
			example.UpdatedAt = example.CreatedAt
			s.IssueComments = append(s.IssueComments, example)
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State != "cooldown_observed" {
				t.Fatal("hidden command example closed window", got)
			}
		})
	}
	for _, mode := range []string{"old-unchanged", "old-edited-before", "old-edited-after", "later-command", "same-time-command"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			c := s.IssueComments[0]
			c.ID = 93
			c.Body = "@coderabbitai review"
			c.CreatedAt = earliest
			c.UpdatedAt = earliest
			switch mode {
			case "old-edited-before":
				c.UpdatedAt = c.UpdatedAt.Add(time.Millisecond)
			case "old-edited-after":
				c.UpdatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
			case "later-command":
				c.CreatedAt = s.IssueComments[1].CreatedAt.Add(time.Second)
				c.UpdatedAt = c.CreatedAt
			case "same-time-command":
				c.CreatedAt = s.IssueComments[0].CreatedAt
				c.UpdatedAt = c.CreatedAt
			}
			s.IssueComments = append(s.IssueComments, c)
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			blocked := mode == "old-edited-after" || mode == "same-time-command"
			if (got.State == "cooldown_observed") == blocked {
				t.Fatal("command attribution timing wrong", mode, got)
			}
		})
	}
}

func TestHostedAllowanceInputAndCollectionBounds(t *testing.T) {
	s, intent, poster, bot, earliest := allowanceFixture(t)
	for _, bad := range []HostedTriggerIntent{{OperationID: "bad", AttemptID: intent.AttemptID, HeadSHA: intent.HeadSHA}, {OperationID: intent.OperationID, AttemptID: "bad", HeadSHA: intent.HeadSHA}, {OperationID: intent.OperationID, AttemptID: intent.AttemptID, HeadSHA: strings.ToUpper(intent.HeadSHA)}} {
		if body, err := HostedAllowanceQueryBody(bad); err == nil || body != "" {
			t.Fatal("invalid query binding accepted")
		}
		if _, err := ResolveHostedAllowance(s, bad, poster, bot, earliest); err == nil {
			t.Fatal("invalid resolver binding accepted")
		}
	}
	for _, bad := range []Author{{}, poster, {ID: bot.ID, Login: "", Kind: "Bot"}} {
		if _, err := ResolveHostedAllowance(s, intent, poster, bad, earliest); err == nil {
			t.Fatal("invalid/same provider identity accepted")
		}
	}
	for _, times := range [][2]time.Time{{{}, s.ObservedAt}, {earliest, {}}, {s.ObservedAt, earliest}, {earliest.In(time.FixedZone("offset", 3600)), s.ObservedAt}} {
		copy := s
		copy.ObservedAt = times[1]
		if _, err := ResolveHostedAllowance(copy, intent, poster, bot, times[0]); err == nil {
			t.Fatal("invalid trusted times accepted")
		}
	}
	for _, mode := range []string{"duplicateID", "zeroID", "nulltime", "reversedtime", "invalidactor", "invalidutf8", "controls", "bodylimit", "countlimit", "aggregate"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			switch mode {
			case "duplicateID":
				s.IssueComments[1].ID = s.IssueComments[0].ID
			case "zeroID":
				s.IssueComments[1].ID = 0
			case "nulltime":
				s.IssueComments[1].UpdatedAt = time.Time{}
			case "reversedtime":
				s.IssueComments[1].UpdatedAt = earliest
			case "invalidactor":
				s.IssueComments[1].Author = Author{}
			case "invalidutf8":
				s.IssueComments[1].Body = "\xff"
			case "controls":
				s.IssueComments[1].Body = "\x00"
			case "bodylimit":
				s.IssueComments[1].Body = strings.Repeat("x", githubMaxText+1)
			case "countlimit":
				s.IssueComments = make([]IssueComment, githubMaxItems+1)
			case "aggregate":
				for i := 0; i < 257; i++ {
					c := s.IssueComments[0]
					c.ID = int64(1000 + i)
					c.Body = strings.Repeat("x", githubMaxText)
					s.IssueComments = append(s.IssueComments, c)
				}
			}
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State != "ambiguous" || !got.RetryNotBefore.IsZero() {
				t.Fatal("malformed/overbound history accepted", mode, got)
			}
		})
	}
}
