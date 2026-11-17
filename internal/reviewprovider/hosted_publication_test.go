package reviewprovider

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const publicationTestID = "12345678-1234-1234-1234-123456789abc"
const publicationAttemptID = "abcdefab-abcd-abcd-abcd-abcdefabcdef"

func publicationTestMarker(ordinal, total int) string {
	return fmt.Sprintf("<!-- coderabbit-review-publication v1 publication=%s attempt=%s batch=%d/%d -->", publicationTestID, publicationAttemptID, ordinal, total)
}

func publicationFixture(t *testing.T) (HostedPublicationSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	intent, poster, earliest, observed, trigger := triggerFixture(t)
	bot := Author{ID: 999, Login: "coderabbitai[bot]", Kind: "Bot"}
	finished := trigger.CreatedAt.Add(2 * time.Second)
	review := HostedReview{ID: 1001, Body: "**Actionable comments posted: 1**\n\nSummary-only/outside-diff text <b>must stay literal</b>.\n" + publicationTestMarker(1, 1), State: "commented", HeadSHA: intent.HeadSHA, Author: bot, SubmittedAt: finished}
	line := int64(3)
	root := ReviewComment{ID: 2001, ReviewID: review.ID, Body: "Exact retained root body", Author: bot, Path: "source.go", Line: &line, HeadSHA: intent.HeadSHA, OriginalHeadSHA: intent.HeadSHA, CreatedAt: finished.Add(-time.Second), UpdatedAt: finished.Add(-time.Second)}
	return HostedPublicationSnapshot{IssueComments: []IssueComment{trigger}, Reviews: []HostedReview{review}, ReviewComments: []ReviewComment{root}, Complete: true, ObservedAt: observed}, intent, poster, bot, earliest
}

func resolvePublication(t *testing.T, s HostedPublicationSnapshot, intent HostedTriggerIntent, poster, bot Author, earliest time.Time, state, reason string) HostedPublication {
	t.Helper()
	got, err := ResolveHostedPublication(s, intent, poster, bot, earliest)
	if err != nil || got.State != state || reason != "" && got.Reason != reason {
		t.Fatalf("got %s/%s want %s/%s: %v", got.State, got.Reason, state, reason, err)
	}
	return got
}

func TestHostedPublicationCompleteBatchesRetainRootsAndAllSummaryText(t *testing.T) {
	s, intent, poster, bot, earliest := publicationFixture(t)
	s.Reviews[0].Body = strings.Replace(s.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
	second := s.Reviews[0]
	second.ID = 1002
	second.Body = "Additional summary-only and outside-diff evidence.\n" + publicationTestMarker(2, 2)
	second.SubmittedAt = second.SubmittedAt.Add(time.Second)
	extra := second
	extra.ID = 1003
	extra.Body = "Markerless eligible summary MUST remain available."
	extra.SubmittedAt = extra.SubmittedAt.Add(time.Second)
	s.Reviews = []HostedReview{second, extra, s.Reviews[0]}
	reply := s.ReviewComments[0]
	reply.ID = 2002
	reply.InReplyToID = 2001
	reply.Body = "Human reply is not another actionable root"
	reply.Author = poster
	s.ReviewComments = append(s.ReviewComments, reply)
	got := resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "publication_inventory_attributed")
	if got.PublicationID != publicationTestID || got.ProviderAttemptID != publicationAttemptID || got.ProviderAttemptID == intent.AttemptID || len(got.Batches) != 2 || got.Batches[0].ID != 1001 || got.Batches[1].ID != 1002 || got.FinishedAt != second.SubmittedAt || len(got.Roots) != 1 || got.Roots[0].ID != 2001 || len(got.EligibleReviews) != 3 {
		t.Fatal("batch/root inventory lost stable identities")
	}
	if got.Batches[0].Body != s.Reviews[2].Body || got.Batches[1].Body != second.Body || got.EligibleReviews[2].Body != extra.Body || got.Roots[0].Body != s.ReviewComments[0].Body {
		t.Fatal("publication transformed/discarded summary, marker or root text")
	}
	*got.Roots[0].Line = 88
	if *s.ReviewComments[0].Line != 3 {
		t.Fatal("resolver returned caller-owned line pointer")
	}
}

func TestHostedPublicationZeroRootsAndStableActorRename(t *testing.T) {
	s, intent, poster, bot, earliest := publicationFixture(t)
	s.Reviews[0].Body = "**Actionable comments posted: 0**\nOutside-diff summary remains unclassified\n" + publicationTestMarker(1, 1)
	s.ReviewComments = nil
	s.Reviews[0].Author.Login = "renamed-provider"
	s.IssueComments[0].Author.Login = "renamed-posting-account"
	for _, state := range []string{"commented", "approved", "changes_requested"} {
		s.Reviews[0].State = state
		got := resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
		if len(got.Roots) != 0 || got.Batches[0].Body != s.Reviews[0].Body {
			t.Fatal("zero inventory discarded summary or invented roots")
		}
	}
}

func TestHostedPublicationMissingAndUnattributableEvidenceNeverCompletes(t *testing.T) {
	tests := []struct {
		name, state, reason string
		change              func(*HostedPublicationSnapshot)
	}{
		{"incomplete collection", "uncertain", "incomplete_collection", func(s *HostedPublicationSnapshot) { s.Complete = false }},
		{"missing trigger", "uncertain", "trigger_not_proven", func(s *HostedPublicationSnapshot) { s.IssueComments = nil }},
		{"markerless", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) {
			s.Reviews[0].Body = "**Actionable comments posted: 1**\nmarkerless result"
		}},
		{"status chatter", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) { s.Reviews[0].Body = "Review in progress" }},
		{"empty review", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) { s.Reviews[0].Body = "" }},
		{"pending review", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) {
			s.Reviews[0].State = "pending"
			s.Reviews[0].SubmittedAt = time.Time{}
		}},
		{"dismissed review", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) { s.Reviews[0].State = "dismissed" }},
		{"wrong actor ID same login", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) { s.Reviews[0].Author.ID++ }},
		{"wrong actor kind", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) { s.Reviews[0].Author.Kind = "User" }},
		{"wrong head", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) { s.Reviews[0].HeadSHA = strings.Repeat("b", 40) }},
		{"before actual trigger", "active", "publication_not_proven", func(s *HostedPublicationSnapshot) {
			s.Reviews[0].SubmittedAt = s.IssueComments[0].CreatedAt.Add(-time.Second)
		}},
		{"missing batch", "active", "missing_publication_batches", func(s *HostedPublicationSnapshot) {
			s.Reviews[0].Body = strings.Replace(s.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, intent, poster, bot, earliest := publicationFixture(t)
			test.change(&s)
			resolvePublication(t, s, intent, poster, bot, earliest, test.state, test.reason)
		})
	}
}

func TestHostedPublicationMarkersMustBeCanonicalAndVisible(t *testing.T) {
	for _, wrapper := range []struct {
		name string
		wrap func(string) string
	}{
		{"backtick fence", func(s string) string { return "```text\n" + s + "\n```" }},
		{"tilde fence", func(s string) string { return "~~~~\n" + s + "\n~~~~" }},
		{"quoted", func(s string) string { return "> " + s }},
		{"indented code", func(s string) string { return "    " + s }},
		{"outer HTML comment", func(s string) string { return "<!-- hidden comment\n" + s + "\n-->" }},
		{"html pre", func(s string) string { return "<pre>\n" + s + "\n</pre>" }},
		{"html quote", func(s string) string { return "<blockquote>\n" + s + "\n</blockquote>" }},
		{"nested html quote", func(s string) string { return "<blockquote>\n<blockquote>\n</blockquote>\n" + s + "\n</blockquote>" }},
	} {
		t.Run(wrapper.name, func(t *testing.T) {
			s, intent, poster, bot, earliest := publicationFixture(t)
			s.Reviews[0].Body = "**Actionable comments posted: 1**\n" + wrapper.wrap(publicationTestMarker(1, 1))
			resolvePublication(t, s, intent, poster, bot, earliest, "active", "publication_not_proven")
		})
	}
	for _, bad := range []string{
		strings.ToUpper(publicationTestMarker(1, 1)),
		strings.Replace(publicationTestMarker(1, 1), "v1", "v2", 1),
		publicationTestMarker(0, 1), publicationTestMarker(2, 1), publicationTestMarker(1, 201),
		"prefix " + publicationTestMarker(1, 1), publicationTestMarker(1, 1) + " suffix",
		publicationTestMarker(1, 1) + "\n" + publicationTestMarker(1, 1),
	} {
		s, intent, poster, bot, earliest := publicationFixture(t)
		s.Reviews[0].Body = "**Actionable comments posted: 1**\n" + bad
		got, err := ResolveHostedPublication(s, intent, poster, bot, earliest)
		if err != nil || got.State == "attributed" {
			t.Fatal("malformed marker completed publication", got.State, err)
		}
	}
	s, intent, poster, bot, earliest := publicationFixture(t)
	s.Reviews[0].Body = "```\n" + publicationTestMarker(2, 4) + "\n```\n" + s.Reviews[0].Body
	resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
}

func TestHostedPublicationConflictsCountsAndRootBindings(t *testing.T) {
	tests := map[string]func(*HostedPublicationSnapshot){
		"duplicate batch": func(s *HostedPublicationSnapshot) { r := s.Reviews[0]; r.ID++; s.Reviews = append(s.Reviews, r) },
		"competing publication": func(s *HostedPublicationSnapshot) {
			r := s.Reviews[0]
			r.ID++
			r.Body = strings.Replace(r.Body, publicationTestID, "22345678-1234-1234-1234-123456789abc", 1)
			s.Reviews = append(s.Reviews, r)
		},
		"competing provider attempt": func(s *HostedPublicationSnapshot) {
			r := s.Reviews[0]
			r.ID++
			r.Body = strings.Replace(r.Body, publicationAttemptID, "bbcdefab-abcd-abcd-abcd-abcdefabcdef", 1)
			s.Reviews = append(s.Reviews, r)
		},
		"conflicting totals": func(s *HostedPublicationSnapshot) {
			r := s.Reviews[0]
			r.ID++
			r.Body = publicationTestMarker(2, 2)
			s.Reviews = append(s.Reviews, r)
		},
		"missing count": func(s *HostedPublicationSnapshot) { s.Reviews[0].Body = publicationTestMarker(1, 1) },
		"duplicate count": func(s *HostedPublicationSnapshot) {
			s.Reviews[0].Body = "**Actionable comments posted: 1**\n" + s.Reviews[0].Body
		},
		"count exceeds bound": func(s *HostedPublicationSnapshot) {
			s.Reviews[0].Body = strings.Replace(s.Reviews[0].Body, "posted: 1", "posted: 201", 1)
		},
		"count mismatch":          func(s *HostedPublicationSnapshot) { s.ReviewComments = nil },
		"root on foreign review":  func(s *HostedPublicationSnapshot) { s.ReviewComments[0].ReviewID++ },
		"reply cannot count root": func(s *HostedPublicationSnapshot) { s.ReviewComments[0].InReplyToID = 999 },
		"foreign root actor":      func(s *HostedPublicationSnapshot) { s.ReviewComments[0].Author.ID++ },
		"foreign root kind":       func(s *HostedPublicationSnapshot) { s.ReviewComments[0].Author.Kind = "User" },
		"moved original head":     func(s *HostedPublicationSnapshot) { s.ReviewComments[0].OriginalHeadSHA = strings.Repeat("b", 40) },
		"root before trigger": func(s *HostedPublicationSnapshot) {
			s.ReviewComments[0].CreatedAt = s.IssueComments[0].CreatedAt.Add(-time.Second)
		},
		"root after finish": func(s *HostedPublicationSnapshot) {
			s.ReviewComments[0].CreatedAt = s.Reviews[0].SubmittedAt.Add(time.Second)
			s.ReviewComments[0].UpdatedAt = s.ReviewComments[0].CreatedAt
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := publicationFixture(t)
			change(&s)
			resolvePublication(t, s, intent, poster, bot, earliest, "ambiguous", "")
		})
	}
	s, intent, poster, bot, earliest := publicationFixture(t)
	s.ReviewComments[0].HeadSHA = strings.Repeat("b", 40) // Original identity survives a later displayed-head change.
	resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
}

func TestHostedPublicationCompetingCommandsCloseTriggerWindow(t *testing.T) {
	for _, command := range []string{"@coderabbitai full review", "@coderabbitai review", "please @coderabbitai resume"} {
		for _, timing := range []string{"before trigger", "between", "after finish", "edited older"} {
			t.Run(command+timing, func(t *testing.T) {
				s, intent, poster, bot, earliest := publicationFixture(t)
				c := s.IssueComments[0]
				c.ID++
				c.Body = command
				c.Author = Author{ID: 777, Login: "another-user", Kind: "User"}
				switch timing {
				case "before trigger":
					c.CreatedAt = c.CreatedAt.Add(-time.Second)
				case "between":
					c.CreatedAt = c.CreatedAt.Add(time.Second)
				case "after finish":
					c.CreatedAt = s.Reviews[0].SubmittedAt.Add(time.Second)
				case "edited older":
					c.CreatedAt = c.CreatedAt.Add(-time.Second)
				}
				c.UpdatedAt = c.CreatedAt
				if timing == "edited older" {
					c.UpdatedAt = s.Reviews[0].SubmittedAt.Add(time.Second)
				}
				s.IssueComments = append(s.IssueComments, c)
				want := "attributed"
				if timing == "between" || timing == "edited older" {
					want = "uncertain"
				}
				resolvePublication(t, s, intent, poster, bot, earliest, want, "")
			})
		}
	}
}

func TestHostedPublicationBoundsMalformedInputsAndNoMutation(t *testing.T) {
	tests := map[string]func(*HostedPublicationSnapshot){
		"duplicate issue ID":   func(s *HostedPublicationSnapshot) { s.IssueComments = append(s.IssueComments, s.IssueComments[0]) },
		"duplicate review ID":  func(s *HostedPublicationSnapshot) { s.Reviews = append(s.Reviews, s.Reviews[0]) },
		"duplicate root ID":    func(s *HostedPublicationSnapshot) { s.ReviewComments = append(s.ReviewComments, s.ReviewComments[0]) },
		"body overflow":        func(s *HostedPublicationSnapshot) { s.Reviews[0].Body = strings.Repeat("x", githubMaxText+1) },
		"invalid UTF8":         func(s *HostedPublicationSnapshot) { s.Reviews[0].Body = string([]byte{255}) },
		"future submitted":     func(s *HostedPublicationSnapshot) { s.Reviews[0].SubmittedAt = s.ObservedAt.Add(time.Second) },
		"invalid review state": func(s *HostedPublicationSnapshot) { s.Reviews[0].State = "COMMENTED" },
		"reversed comment times": func(s *HostedPublicationSnapshot) {
			s.ReviewComments[0].UpdatedAt = s.ReviewComments[0].CreatedAt.Add(-time.Second)
		},
		"invalid reply ID": func(s *HostedPublicationSnapshot) { s.ReviewComments[0].InReplyToID = -1 },
		"item overflow":    func(s *HostedPublicationSnapshot) { s.Reviews = make([]HostedReview, githubMaxItems+1) },
		"aggregate overflow": func(s *HostedPublicationSnapshot) {
			base := s.Reviews[0]
			s.Reviews = nil
			for i := 0; i < 257; i++ {
				r := base
				r.ID = int64(i + 1)
				r.Body = strings.Repeat("a", githubMaxText)
				s.Reviews = append(s.Reviews, r)
			}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := publicationFixture(t)
			change(&s)
			got := resolvePublication(t, s, intent, poster, bot, earliest, "ambiguous", "")
			if strings.Contains(got.Reason, "Exact retained") {
				t.Fatal("diagnostic exposed private body")
			}
		})
	}
	s, intent, poster, bot, earliest := publicationFixture(t)
	before := s.Reviews[0]
	got := resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
	if !reflect.DeepEqual(s.Reviews[0], before) || got.Trigger.State != "adopted" {
		t.Fatal("pure resolver changed input or lost trigger provenance")
	}
	bot.ID = 0
	if _, err := ResolveHostedPublication(s, intent, poster, bot, earliest); err == nil {
		t.Fatal("invalid configured actor accepted")
	}
}

func TestHostedPublicationMaximumBatchInventoryAndHiddenCount(t *testing.T) {
	s, intent, poster, bot, earliest := publicationFixture(t)
	first := s.Reviews[0]
	s.Reviews = nil
	s.ReviewComments = nil
	for i := 1; i <= 200; i++ {
		review := first
		review.ID = int64(1000 + i)
		review.Body = publicationTestMarker(i, 200)
		if i == 1 {
			review.Body = "**Actionable comments posted: 0**\n" + review.Body
		}
		s.Reviews = append(s.Reviews, review)
	}
	got := resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
	if len(got.Batches) != 200 || len(got.Roots) != 0 {
		t.Fatal("maximum batch inventory lost entries")
	}
	s.Reviews[0].Body = "```\n**Actionable comments posted: 0**\n```\n" + publicationTestMarker(1, 200)
	resolvePublication(t, s, intent, poster, bot, earliest, "ambiguous", "invalid_actionable_count")
}

func TestHostedPublicationSameTimestampAsTriggerCannotEstablishOrdering(t *testing.T) {
	s, intent, poster, bot, earliest := publicationFixture(t)
	s.Reviews[0].SubmittedAt = s.IssueComments[0].CreatedAt
	s.Reviews[0].Body = "**Actionable comments posted: 0**\n" + publicationTestMarker(1, 1)
	s.ReviewComments = nil
	resolvePublication(t, s, intent, poster, bot, earliest, "active", "publication_not_proven")
	// The next observable timestamp establishes ordering; root comments may share
	// the trigger's timestamp because the exact review ID binds their inventory.
	s.Reviews[0].SubmittedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
	resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
	s.Reviews[0].Body = "**Actionable comments posted: 1**\n" + publicationTestMarker(1, 1)
	s.ReviewComments = []ReviewComment{{ID: 2001, ReviewID: s.Reviews[0].ID, Body: "Exact root", Author: bot, Path: "source.go", HeadSHA: intent.HeadSHA, OriginalHeadSHA: intent.HeadSHA, CreatedAt: s.IssueComments[0].CreatedAt, UpdatedAt: s.IssueComments[0].CreatedAt}}
	resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
}

func TestHostedPublicationPreservesLocationsWithIndependentPointerCopies(t *testing.T) {
	s, intent, poster, bot, earliest := publicationFixture(t)
	start, original, originalStart := int64(2), int64(13), int64(12)
	s.ReviewComments[0].StartLine = &start
	s.ReviewComments[0].OriginalLine = &original
	s.ReviewComments[0].OriginalStartLine = &originalStart
	s.ReviewComments[0].Side, s.ReviewComments[0].StartSide = "RIGHT", "RIGHT"
	got := resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
	if len(got.Roots) != 1 || !reflect.DeepEqual(got.Roots[0], s.ReviewComments[0]) {
		t.Fatal("location facts or body lost from attributed root")
	}
	copied := got.Roots[0]
	pairs := []struct{ source, returned *int64 }{
		{s.ReviewComments[0].Line, copied.Line},
		{s.ReviewComments[0].StartLine, copied.StartLine},
		{s.ReviewComments[0].OriginalLine, copied.OriginalLine},
		{s.ReviewComments[0].OriginalStartLine, copied.OriginalStartLine},
	}
	for _, pair := range pairs {
		if pair.source == pair.returned || pair.source == nil || pair.returned == nil {
			t.Fatal("publication leaked a caller-owned numeric pointer")
		}
		before := *pair.source
		*pair.returned = 90
		if *pair.source != before {
			t.Fatal("returned location mutation changed source evidence")
		}
		*pair.returned = before
		*pair.source = 91
		if *pair.returned != before {
			t.Fatal("source location mutation changed returned evidence")
		}
		*pair.source = before
	}
	// An outdated current location does not erase known original coordinates.
	s.ReviewComments[0].Line, s.ReviewComments[0].StartLine = nil, nil
	got = resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
	if got.Roots[0].Line != nil || got.Roots[0].StartLine != nil || got.Roots[0].OriginalLine == nil || *got.Roots[0].OriginalLine != original || got.Roots[0].OriginalStartLine == nil || *got.Roots[0].OriginalStartLine != originalStart {
		t.Fatal("outdated current location overwrote original coordinates")
	}
}

func TestHostedPublicationOptionalLocationsValidateWithoutInferringRanges(t *testing.T) {
	for _, field := range []string{"line", "start", "original", "original start"} {
		for _, invalid := range []int64{0, -1, 1 << 31} {
			t.Run(fmt.Sprint(field, "/", invalid), func(t *testing.T) {
				s, intent, poster, bot, earliest := publicationFixture(t)
				switch field {
				case "line":
					s.ReviewComments[0].Line = &invalid
				case "start":
					s.ReviewComments[0].StartLine = &invalid
				case "original":
					s.ReviewComments[0].OriginalLine = &invalid
				default:
					s.ReviewComments[0].OriginalStartLine = &invalid
				}
				resolvePublication(t, s, intent, poster, bot, earliest, "ambiguous", "invalid_publication_evidence")
			})
		}
	}
	for _, side := range []string{"left", "CENTER", "RIGHT "} {
		for _, start := range []bool{false, true} {
			s, intent, poster, bot, earliest := publicationFixture(t)
			if start {
				s.ReviewComments[0].StartSide = side
			} else {
				s.ReviewComments[0].Side = side
			}
			resolvePublication(t, s, intent, poster, bot, earliest, "ambiguous", "invalid_publication_evidence")
		}
	}
	s, intent, poster, bot, earliest := publicationFixture(t)
	maximum := int64(1<<31 - 1)
	s.ReviewComments[0].StartLine, s.ReviewComments[0].OriginalLine, s.ReviewComments[0].OriginalStartLine = &maximum, &maximum, &maximum
	s.ReviewComments[0].Side, s.ReviewComments[0].StartSide = "LEFT", "RIGHT"
	resolvePublication(t, s, intent, poster, bot, earliest, "attributed", "")
}

func TestHostedPublicationEditedCommentsCloseUnknownOriginalCommandWindow(t *testing.T) {
	for _, test := range []struct {
		name             string
		created, updated time.Duration
		author           string
		state, reason    string
	}{
		{"removed command edited after review", time.Second, 3 * time.Second, "other", "uncertain", "attribution_window_closed"},
		{"older comment edited after trigger", -2 * time.Second, 3 * time.Second, "other", "uncertain", "attribution_window_closed"},
		{"older comment edited at trigger", -2 * time.Second, 0, "other", "uncertain", "attribution_window_closed"},
		{"same posting actor different comment", time.Second, 3 * time.Second, "poster", "uncertain", "attribution_window_closed"},
		{"review at possible command time", 2 * time.Second, 3 * time.Second, "other", "uncertain", "attribution_window_closed"},
		{"unedited benign comment", time.Second, time.Second, "other", "attributed", "publication_inventory_attributed"},
		{"older comment edited before trigger", -3 * time.Second, -time.Second, "other", "attributed", "publication_inventory_attributed"},
		{"provider comment edit", time.Second, 3 * time.Second, "provider", "attributed", "publication_inventory_attributed"},
		{"review before earliest possible competing command", 3 * time.Second, 4 * time.Second, "other", "attributed", "publication_inventory_attributed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, intent, poster, bot, earliest := publicationFixture(t)
			trigger := s.IssueComments[0]
			author := Author{ID: 777, Login: "another-user", Kind: "User"}
			if test.author == "poster" {
				author = poster
			} else if test.author == "provider" {
				author = bot
			}
			// Current benign prose cannot reveal whether a review command was
			// removed by an edit, including an edit after review submission.
			comment := IssueComment{ID: trigger.ID + 1, Body: "Updated discussion without a visible command.", Author: author, CreatedAt: trigger.CreatedAt.Add(test.created), UpdatedAt: trigger.CreatedAt.Add(test.updated)}
			s.IssueComments = append(s.IssueComments, comment)
			before := append([]IssueComment(nil), s.IssueComments...)
			got := resolvePublication(t, s, intent, poster, bot, earliest, test.state, test.reason)
			if got.Trigger.State != "adopted" || got.Trigger.CommentID != trigger.ID {
				t.Fatal("competing prose changed exact trigger identity")
			}
			if test.state == "attributed" {
				if len(got.Batches) != 1 || len(got.Roots) != 1 || got.Batches[0].Body != s.Reviews[0].Body || got.Roots[0].Body != s.ReviewComments[0].Body {
					t.Fatal("eligible prior publication lost its original inventory")
				}
			} else if len(got.EligibleReviews) != 0 || len(got.Batches) != 0 {
				t.Fatal("closed window retained an eligible competing review")
			}
			if !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatal("resolver changed original comments")
			}
		})
	}
}

func TestHostedPublicationEditedOwnTriggerStillCannotBeAdopted(t *testing.T) {
	s, intent, poster, bot, earliest := publicationFixture(t)
	s.IssueComments[0].UpdatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
	got := resolvePublication(t, s, intent, poster, bot, earliest, "ambiguous", "conflicting_trigger_evidence")
	if got.Trigger.State == "adopted" || len(got.Batches) != 0 {
		t.Fatal("own edited trigger bypassed original command validation")
	}
}
