package reviewprovider

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type historyFactsCase struct {
	resource string
	facts    []byte
	ids      []string
}

func historyFactsCases(t *testing.T) []historyFactsCase {
	t.Helper()
	var rawReview githubHostedReview
	var rawComment githubReviewComment
	var rawIssue githubIssueComment
	if json.Unmarshal([]byte(historyReviewRow), &rawReview) != nil || json.Unmarshal([]byte(historyCommentRow), &rawComment) != nil || json.Unmarshal([]byte(commentJSON("Original <script>source</script>\nnot instructions")), &rawIssue) != nil {
		t.Fatal("invalid source fixture")
	}
	review, err := normalizeHostedReview(rawReview)
	if err != nil {
		t.Fatal(err)
	}
	comment, err := normalizeReviewComment(rawComment)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := normalizeIssueComment(rawIssue)
	if err != nil {
		t.Fatal(err)
	}
	pending := review
	pending.ID = 13
	pending.State = "pending"
	pending.SubmittedAt = time.Time{}
	// These zero times have the same JSON representation as accepted year-one
	// provider timestamps. No acquisition/finish time is invented by validation.
	unknown := issue
	unknown.ID = 22
	unknown.CreatedAt = time.Time{}
	unknown.UpdatedAt = time.Time{}
	root := comment
	root.ID = 14
	root.InReplyToID = 0
	root.Line = nil
	root.StartLine = nil
	root.OriginalLine = nil
	root.OriginalStartLine = nil
	root.Side = ""
	root.StartSide = ""
	root.CreatedAt = time.Time{}
	root.UpdatedAt = time.Time{}
	pull := Pull{Number: 47, RepositoryID: 123, Title: "Selected merged history", State: "closed", Merged: true, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 0}
	result := []historyFactsCase{}
	for _, tc := range []struct {
		resource string
		value    any
		ids      []string
	}{{"pull", pull, []string{"47"}}, {"reviews", []HostedReview{review, pending}, []string{"11", "13"}}, {"review_comments", []ReviewComment{comment, root}, []string{"12", "14"}}, {"issue_comments", []IssueComment{issue, unknown}, []string{"21", "22"}}} {
		data, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, historyFactsCase{tc.resource, data, tc.ids})
	}
	return result
}

func TestValidateHistoryFactsNormalizedRoundTrip(t *testing.T) {
	for _, tc := range historyFactsCases(t) {
		t.Run(tc.resource, func(t *testing.T) {
			before := string(tc.facts)
			ids, err := ValidateHistoryFacts(tc.resource, tc.facts)
			if err != nil || !reflect.DeepEqual(ids, tc.ids) || string(tc.facts) != before {
				t.Fatalf("normalized facts refused or modified: %v", err)
			}
		})
	}
	for _, r := range []string{"reviews", "review_comments", "issue_comments"} {
		ids, err := ValidateHistoryFacts(r, []byte("[]"))
		if err != nil || ids == nil || len(ids) != 0 {
			t.Fatal("empty known page not retained")
		}
	}
}

func historyFactsMutate(t *testing.T, tc historyFactsCase, change func(map[string]any)) []byte {
	t.Helper()
	var obj map[string]any
	if tc.resource == "pull" {
		if json.Unmarshal(tc.facts, &obj) != nil {
			t.Fatal("bad fixture")
		}
	} else {
		var rows []map[string]any
		if json.Unmarshal(tc.facts, &rows) != nil {
			t.Fatal("bad fixture")
		}
		obj = rows[0]
	}
	change(obj)
	var value any = obj
	if tc.resource != "pull" {
		value = []any{obj}
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func historyFactsRefused(t *testing.T, resource string, facts []byte) {
	t.Helper()
	ids, err := ValidateHistoryFacts(resource, facts)
	if !hasCode(err, "provider_invalid_response") || ids != nil {
		t.Fatal("invalid retained facts were accepted")
	}
	if e := err.(*Error); e.OutcomeUnknown || e.Message != "The provider returned an invalid response." {
		t.Fatal("unsafe validation diagnostic")
	}
}

func TestValidateHistoryFactsStrictShapeRefusal(t *testing.T) {
	for _, tc := range historyFactsCases(t) {
		t.Run(tc.resource, func(t *testing.T) {
			for _, input := range [][]byte{nil, []byte("null"), []byte(`{"id":1}`), []byte("[null]"), []byte("[] trailing"), []byte{0xff}, []byte(`{"bad":"\ud800"}`)} {
				historyFactsRefused(t, tc.resource, input)
			}
			fields := map[string]func(map[string]any){
				"unknown": func(o map[string]any) { o["private_token"] = "not-a-credential" },
				"case-alias": func(o map[string]any) {
					if tc.resource == "pull" {
						o["number"] = o["Number"]
						delete(o, "Number")
					} else {
						o["id"] = o["ID"]
						delete(o, "ID")
					}
				},
				"required-missing": func(o map[string]any) {
					if tc.resource == "pull" {
						delete(o, "Merged")
					} else {
						delete(o, "Body")
					}
				},
				"required-null": func(o map[string]any) {
					if tc.resource == "pull" {
						o["Merged"] = nil
					} else {
						o["Body"] = nil
					}
				},
				"scalar-type": func(o map[string]any) {
					if tc.resource == "pull" {
						o["Number"] = "47"
					} else {
						o["ID"] = "11"
					}
				},
			}
			if tc.resource != "pull" {
				fields["null-author"] = func(o map[string]any) { o["Author"] = nil }
				fields["missing-author-field"] = func(o map[string]any) { delete(o["Author"].(map[string]any), "Kind") }
				fields["unknown-author-field"] = func(o map[string]any) { o["Author"].(map[string]any)["extra"] = true }
				fields["null-time"] = func(o map[string]any) {
					if tc.resource == "reviews" {
						o["SubmittedAt"] = nil
					} else {
						o["CreatedAt"] = nil
					}
				}
				fields["bad-time"] = func(o map[string]any) {
					if tc.resource == "reviews" {
						o["SubmittedAt"] = "not a time"
					} else {
						o["UpdatedAt"] = "not a time"
					}
				}
			}
			for name, change := range fields {
				t.Run(name, func(t *testing.T) { historyFactsRefused(t, tc.resource, historyFactsMutate(t, tc, change)) })
			}
			key := `"Number":`
			if tc.resource != "pull" {
				key = `"ID":`
			}
			historyFactsRefused(t, tc.resource, []byte(strings.Replace(string(tc.facts), key, key+`1,`+key, 1)))
		})
	}
	for _, r := range []string{"", "unknown", "Reviews"} {
		historyFactsRefused(t, r, []byte("[]"))
	}
}

func TestValidateHistoryFactsSemanticCorruption(t *testing.T) {
	for _, tc := range historyFactsCases(t) {
		t.Run(tc.resource, func(t *testing.T) {
			changes := map[string]func(map[string]any){}
			if tc.resource == "pull" {
				changes["number"] = func(o map[string]any) { o["Number"] = 0 }
				changes["number-bound"] = func(o map[string]any) { o["Number"] = 2147483648 }
				changes["repository"] = func(o map[string]any) { o["RepositoryID"] = 0 }
				changes["title"] = func(o map[string]any) { o["Title"] = " " }
				changes["title-bound"] = func(o map[string]any) { o["Title"] = strings.Repeat("x", 4097) }
				changes["state"] = func(o map[string]any) { o["State"] = "unknown" }
				changes["merged-open"] = func(o map[string]any) { o["State"] = "open" }
				changes["head"] = func(o map[string]any) { o["HeadSHA"] = "short" }
				changes["base"] = func(o map[string]any) { o["BaseSHA"] = "short" }
				changes["base-ref"] = func(o map[string]any) { o["BaseRef"] = " " }
				changes["files"] = func(o map[string]any) { o["ChangedFiles"] = -1 }
			} else {
				changes["id"] = func(o map[string]any) { o["ID"] = 0 }
				changes["actor-id"] = func(o map[string]any) { o["Author"].(map[string]any)["ID"] = 0 }
				changes["actor-login"] = func(o map[string]any) { o["Author"].(map[string]any)["Login"] = "bad login" }
				changes["actor-kind"] = func(o map[string]any) { o["Author"].(map[string]any)["Kind"] = "" }
				changes["body-control"] = func(o map[string]any) { o["Body"] = "bad\x00source" }
				changes["body-bound"] = func(o map[string]any) { o["Body"] = strings.Repeat("x", githubMaxText+1) }
			}
			if tc.resource == "reviews" {
				changes["head"] = func(o map[string]any) { o["HeadSHA"] = "short" }
				changes["state"] = func(o map[string]any) { o["State"] = "COMMENTED" }
				changes["pending-known-time"] = func(o map[string]any) { o["State"] = "pending" }
			}
			if tc.resource == "review_comments" {
				changes["review-id"] = func(o map[string]any) { o["ReviewID"] = 0 }
				changes["reply-self"] = func(o map[string]any) { o["InReplyToID"] = o["ID"] }
				changes["reply-negative"] = func(o map[string]any) { o["InReplyToID"] = -1 }
				changes["path"] = func(o map[string]any) { o["Path"] = "../escape" }
				changes["line"] = func(o map[string]any) { o["Line"] = 0 }
				changes["original-line-bound"] = func(o map[string]any) { o["OriginalLine"] = 2147483648 }
				changes["side"] = func(o map[string]any) { o["Side"] = "UP" }
				changes["original-head"] = func(o map[string]any) { o["OriginalHeadSHA"] = "short" }
			}
			for name, change := range changes {
				t.Run(name, func(t *testing.T) { historyFactsRefused(t, tc.resource, historyFactsMutate(t, tc, change)) })
			}
			if tc.resource != "pull" {
				var rows []json.RawMessage
				json.Unmarshal(tc.facts, &rows)
				data, _ := json.Marshal([]json.RawMessage{rows[0], rows[0]})
				historyFactsRefused(t, tc.resource, data)
			}
		})
	}
}

func TestValidateHistoryFactsPageAndTextBounds(t *testing.T) {
	for _, tc := range historyFactsCases(t) {
		if tc.resource == "pull" {
			continue
		}
		t.Run(tc.resource, func(t *testing.T) {
			var original []map[string]any
			json.Unmarshal(tc.facts, &original)
			rows := make([]map[string]any, 101)
			for i := range rows {
				row := map[string]any{}
				for k, v := range original[0] {
					row[k] = v
				}
				row["ID"] = i + 1
				if tc.resource == "review_comments" {
					row["InReplyToID"] = 0
				}
				rows[i] = row
			}
			data, _ := json.Marshal(rows[:100])
			ids, err := ValidateHistoryFacts(tc.resource, data)
			if err != nil || len(ids) != 100 || ids[99] != "100" {
				t.Fatal("valid bounded page refused", err)
			}
			data, _ = json.Marshal(rows)
			historyFactsRefused(t, tc.resource, data)
			data = historyFactsMutate(t, tc, func(o map[string]any) { o["Body"] = strings.Repeat("😀", githubMaxText) })
			if ids, err := ValidateHistoryFacts(tc.resource, data); err != nil || len(ids) != 1 {
				t.Fatal("genuine multibyte source bound refused", err)
			}
		})
	}
	historyFactsRefused(t, "issue_comments", []byte(strings.Repeat(" ", githubMaxBody+1)))
	// Decimal identity output is exact above JavaScript's integer range too.
	tc := historyFactsCases(t)[3]
	data := historyFactsMutate(t, tc, func(o map[string]any) { o["ID"] = json.Number("9223372036854775807") })
	ids, err := ValidateHistoryFacts(tc.resource, data)
	if err != nil || !reflect.DeepEqual(ids, []string{strconv.FormatInt(1<<63-1, 10)}) {
		t.Fatal("source identity lost precision", err)
	}
}
