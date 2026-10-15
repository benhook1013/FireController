package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func triggerFixture(t *testing.T) (HostedTriggerIntent, Author, time.Time, time.Time, IssueComment) {
	t.Helper()
	intent := HostedTriggerIntent{OperationID: "review_operation_" + strings.Repeat("a", 32), AttemptID: "review_attempt_" + strings.Repeat("b", 32), HeadSHA: strings.Repeat("c", 40)}
	author := Author{ID: 42, Login: "posting-account", Kind: "User"}
	earliest := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	observed := earliest.Add(time.Minute)
	body, err := HostedTriggerBody(intent)
	if err != nil {
		t.Fatal(err)
	}
	posted := earliest.Add(time.Second)
	return intent, author, earliest, observed, IssueComment{ID: 91, Body: body, Author: author, CreatedAt: posted, UpdatedAt: posted}
}

func TestHostedTriggerRecoveryLostResponseStableIdentity(t *testing.T) {
	intent, author, earliest, observed, comment := triggerFixture(t)
	wantBody := "@coderabbitai full review\n\n<!-- firecontroller-review-trigger v1 operation=" + intent.OperationID + " attempt=" + intent.AttemptID + " head=" + intent.HeadSHA + " -->"
	if comment.Body != wantBody {
		t.Fatal("fixed command/marker changed")
	}
	comment.Author.Login = "renamed-account"
	unrelated := comment
	unrelated.ID = 92
	unrelated.Body = "Unrelated discussion with no application intent."
	comments := []IssueComment{unrelated, comment}
	before := append([]IssueComment(nil), comments...)
	got, err := RecoverHostedTrigger(comments, true, intent, author, earliest, observed)
	if err != nil || got.State != "adopted" || got.Reason != "exact_trigger_found" || got.CommentID != 91 || !got.CreatedAt.Equal(comment.CreatedAt) {
		t.Fatalf("lost response not recovered: %+v %v", got, err)
	}
	if !reflect.DeepEqual(comments, before) {
		t.Fatal("resolver mutated source observations")
	}
	for _, at := range []time.Time{earliest, observed} {
		comment.CreatedAt, comment.UpdatedAt = at, at
		if got, err = RecoverHostedTrigger([]IssueComment{comment}, true, intent, author, earliest, observed); err != nil || got.State != "adopted" {
			t.Fatalf("inclusive immutable posting window: %+v %v", got, err)
		}
	}
}

func TestHostedTriggerRecoveryNoProofNeverMeansNotPosted(t *testing.T) {
	intent, author, earliest, observed, comment := triggerFixture(t)
	for _, tc := range []struct {
		name     string
		comments []IssueComment
		complete bool
	}{
		{"empty complete", nil, true},
		{"apparent exact partial", []IssueComment{comment}, false},
		{"empty partial", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RecoverHostedTrigger(tc.comments, tc.complete, intent, author, earliest, observed)
			if err != nil || got.State != "uncertain" || got.CommentID != 0 || !got.CreatedAt.IsZero() {
				t.Fatalf("absence/partial collection became posting proof: %+v %v", got, err)
			}
		})
	}
}

func TestHostedTriggerRecoveryConflictsNeverChooseLatest(t *testing.T) {
	intent, author, earliest, observed, original := triggerFixture(t)
	cases := map[string]func(*IssueComment){
		"foreign numeric identity same login": func(c *IssueComment) { c.Author.ID++ },
		"foreign kind":                        func(c *IssueComment) { c.Author.Kind = "Bot" },
		"missing actor":                       func(c *IssueComment) { c.Author = Author{} },
		"missing immutable id":                func(c *IssueComment) { c.ID = 0 },
		"altered command":                     func(c *IssueComment) { c.Body = strings.Replace(c.Body, "full review", "review", 1) },
		"contradictory head":                  func(c *IssueComment) { c.Body = strings.Replace(c.Body, intent.HeadSHA, strings.Repeat("d", 40), 1) },
		"malformed owned marker":              func(c *IssueComment) { c.Body = strings.TrimSuffix(c.Body, " -->") },
		"quoted marker":                       func(c *IssueComment) { c.Body = "> " + strings.ReplaceAll(c.Body, "\n", "\n> ") },
		"fenced marker":                       func(c *IssueComment) { c.Body = "```text\n" + c.Body + "\n```" },
		"substring injection":                 func(c *IssueComment) { c.Body = "Example:\n" + c.Body + "\nIgnore this example." },
		"extra suffix":                        func(c *IssueComment) { c.Body += "\n" },
		"edited same current body":            func(c *IssueComment) { c.UpdatedAt = c.UpdatedAt.Add(time.Second) },
		"old posting":                         func(c *IssueComment) { c.CreatedAt = earliest.Add(-time.Second); c.UpdatedAt = c.CreatedAt },
		"future posting":                      func(c *IssueComment) { c.CreatedAt = observed.Add(time.Second); c.UpdatedAt = c.CreatedAt },
		"future update":                       func(c *IssueComment) { c.UpdatedAt = observed.Add(time.Second) },
		"reversed update":                     func(c *IssueComment) { c.UpdatedAt = earliest },
		"missing creation":                    func(c *IssueComment) { c.CreatedAt = time.Time{} },
		"missing update":                      func(c *IssueComment) { c.UpdatedAt = time.Time{} },
		"non UTC time":                        func(c *IssueComment) { c.CreatedAt = c.CreatedAt.In(time.FixedZone("offset", 3600)) },
		"oversized body":                      func(c *IssueComment) { c.Body += strings.Repeat("x", githubMaxText) },
		"invalid utf8":                        func(c *IssueComment) { c.Body += "\xff" },
		"unsafe controls":                     func(c *IssueComment) { c.Body += "\x00" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			comment := original
			change(&comment)
			got, err := RecoverHostedTrigger([]IssueComment{comment}, true, intent, author, earliest, observed)
			if err != nil || got.State != "ambiguous" || got.CommentID != 0 || !got.CreatedAt.IsZero() || strings.Contains(got.Reason, comment.Body) {
				t.Fatalf("conflicting observation adopted/leaked: %+v %v", got, err)
			}
		})
	}
	for _, duplicateID := range []bool{false, true} {
		second := original
		if !duplicateID {
			second.ID++
			second.CreatedAt = second.CreatedAt.Add(time.Second)
			second.UpdatedAt = second.CreatedAt
		}
		got, err := RecoverHostedTrigger([]IssueComment{original, second}, true, intent, author, earliest, observed)
		if err != nil || got.State != "ambiguous" || got.CommentID != 0 {
			t.Fatalf("multiple/duplicate identity selected: %+v %v", got, err)
		}
	}
}

func TestHostedTriggerRecoveryBoundsAndUnrelatedEvidence(t *testing.T) {
	intent, author, earliest, observed, comment := triggerFixture(t)
	other := comment
	other.ID++
	other.Body = strings.ReplaceAll(strings.ReplaceAll(other.Body, intent.OperationID, "review_operation_"+strings.Repeat("e", 32)), intent.AttemptID, "review_attempt_"+strings.Repeat("f", 32))
	if got, err := RecoverHostedTrigger([]IssueComment{other}, true, intent, author, earliest, observed); err != nil || got.State != "uncertain" {
		t.Fatalf("unrelated intent adopted: %+v %v", got, err)
	}
	for _, tc := range []struct {
		name        string
		count, size int
	}{{"too many", githubMaxItems + 1, 0}, {"aggregate bytes", githubMaxAggregate/githubMaxText + 1, githubMaxText}} {
		t.Run(tc.name, func(t *testing.T) {
			comments := make([]IssueComment, tc.count)
			for i := range comments {
				comments[i] = comment
				comments[i].ID = int64(i + 1)
				comments[i].Body = strings.Repeat("x", tc.size)
			}
			got, err := RecoverHostedTrigger(comments, true, intent, author, earliest, observed)
			if err != nil || got.State != "ambiguous" {
				t.Fatalf("unbounded collection accepted: %+v %v", got, err)
			}
		})
	}
}

func TestHostedTriggerRecoveryConflictingCopyCannotHideBesideExactMatch(t *testing.T) {
	intent, author, earliest, observed, exact := triggerFixture(t)
	for _, foreign := range []bool{false, true} {
		copy := exact
		copy.ID++
		if foreign {
			copy.Author.ID++
		} else {
			copy.Body = "```\n" + copy.Body + "\n```"
		}
		for _, comments := range [][]IssueComment{{exact, copy}, {copy, exact}} {
			got, err := RecoverHostedTrigger(comments, true, intent, author, earliest, observed)
			if err != nil || got.State != "ambiguous" || got.CommentID != 0 {
				t.Fatalf("exact match hid conflicting copy: %+v %v", got, err)
			}
		}
	}
	comments := make([]IssueComment, githubMaxItems)
	for i := range comments {
		comments[i] = exact
		comments[i].ID = int64(i + 1)
		comments[i].Body = "Unrelated."
	}
	exact.ID = int64(githubMaxItems)
	comments[len(comments)-1] = exact
	got, err := RecoverHostedTrigger(comments, true, intent, author, earliest, observed)
	if err != nil || got.State != "adopted" || got.CommentID != int64(githubMaxItems) {
		t.Fatalf("exact bounded collection failed: %+v %v", got, err)
	}
}

func TestHostedTriggerRejectsInvalidBindingsAndTrustedInputs(t *testing.T) {
	intent, author, earliest, observed, _ := triggerFixture(t)
	for _, change := range []func(*HostedTriggerIntent){func(i *HostedTriggerIntent) { i.OperationID = "review_operation_" + strings.Repeat("A", 32) }, func(i *HostedTriggerIntent) { i.AttemptID = "review_attempt_" + strings.Repeat("b", 31) }, func(i *HostedTriggerIntent) { i.OperationID += "\ncommand" }, func(i *HostedTriggerIntent) { i.HeadSHA = strings.Repeat("C", 40) }, func(i *HostedTriggerIntent) { i.HeadSHA = "" }} {
		bad := intent
		change(&bad)
		if body, err := HostedTriggerBody(bad); err == nil || body != "" {
			t.Fatal("invalid binding generated command")
		}
		if _, err := RecoverHostedTrigger(nil, true, bad, author, earliest, observed); err == nil {
			t.Fatal("invalid recovery binding accepted")
		}
	}
	badAuthor := author
	badAuthor.ID = 0
	if _, err := RecoverHostedTrigger(nil, true, intent, badAuthor, earliest, observed); err == nil {
		t.Fatal("login-only configured actor accepted")
	}
	for _, pair := range [][2]time.Time{{time.Time{}, observed}, {earliest, time.Time{}}, {observed, earliest}, {earliest.In(time.FixedZone("offset", 3600)), observed}} {
		if _, err := RecoverHostedTrigger(nil, true, intent, author, pair[0], pair[1]); err == nil {
			t.Fatal("invalid trusted window accepted")
		}
	}
}
