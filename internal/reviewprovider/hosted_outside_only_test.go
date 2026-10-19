package reviewprovider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func outsideOnlyFixture(t *testing.T, strong bool) hfFixture {
	t.Helper()
	item := hfOutside(1, "engine/events.go", "19", "Keep shared event wording accurate.")
	if strong {
		item = strings.ReplaceAll(strings.ReplaceAll(item, "<em>", "<strong>"), "</em>", "</strong>")
	}
	f := hfNew(t, nil, hfOutsideCategory(1, item))
	f.snapshot.Reviews[0].Body = strings.TrimPrefix(f.snapshot.Reviews[0].Body, "**Actionable comments posted: 0**\n\n")
	f.snapshot.ReviewComments = []ReviewComment{}
	return f
}

func TestHostedOutsideOnlyOmittedInlineCount(t *testing.T) {
	for _, strong := range []bool{false, true} {
		name := "em"
		if strong {
			name = "strong"
		}
		t.Run(name, func(t *testing.T) {
			f := outsideOnlyFixture(t, strong)
			before, _ := json.Marshal(f.snapshot)
			got := f.extract(t)
			if !got.Complete || got.Publication.State != "attributed" || got.Reason != "finding_inventory_complete" || len(got.Findings) != 1 || len(got.Publication.Roots) != 0 || len(got.Publication.Batches) != 1 {
				t.Fatalf("outside-only inventory not attributed: state=%s reason=%s complete=%t findings=%d", got.Publication.State, got.Reason, got.Complete, len(got.Findings))
			}
			finding := got.Findings[0]
			if finding.Kind != "outside_diff" || finding.ReviewID != 401 || finding.CommentID != 0 || finding.Path != "engine/events.go" || finding.Severity != "low" || finding.Title != "Keep shared event wording accurate." || finding.StartLine == nil || *finding.StartLine != 19 || finding.Fingerprint != "000000000000000000000001" || !strings.Contains(f.snapshot.Reviews[0].Body, finding.Detail) {
				t.Fatalf("finding identity or literal detail changed: %+v", finding)
			}
			after, _ := json.Marshal(f.snapshot)
			if !reflect.DeepEqual(before, after) || got.Publication.Batches[0].Body != f.snapshot.Reviews[0].Body {
				t.Fatal("source body changed")
			}
		})
	}
}

func TestHostedOutsideOnlyOmissionNeverHidesUnknownInventory(t *testing.T) {
	tests := map[string]func(*hfFixture){
		"empty primary with markerless outside review": func(f *hfFixture) {
			other := f.snapshot.Reviews[0]
			other.ID++
			other.Body = hfOutsideCategory(1, hfOutside(2, "engine/other.go", "20", "Other eligible summary."))
			f.snapshot.Reviews[0].Body = publicationTestMarker(1, 1)
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		},
		"empty": func(f *hfFixture) { f.snapshot.Reviews[0].Body = publicationTestMarker(1, 1) },
		"zero outside": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = hfOutsideCategory(0) + "\n" + publicationTestMarker(1, 1)
		},
		"duplicate only": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = hfDuplicateCategory(1, hfDuplicateGroup("engine/events.go", 1, hfDuplicateItem(1, "19", "Duplicate detail."))) + "\n" + publicationTestMarker(1, 1)
		},
		"hidden outside": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "```\n" + f.snapshot.Reviews[0].Body + "\n```\n" + publicationTestMarker(1, 1)
		},
		"malformed inline count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: unknown**\n" + f.snapshot.Reviews[0].Body
		},
		"noncanonical inline count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: 00**\n" + f.snapshot.Reviews[0].Body
		},
		"repeated inline count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: 0**\n**Actionable comments posted: 0**\n" + f.snapshot.Reviews[0].Body
		},
		"conflicting inline count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: 0**\n**Actionable comments posted: 1**\n" + f.snapshot.Reviews[0].Body
		},
		"missing fingerprint": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, hfMarker(1), "", 1)
		},
		"wrong category count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "comments (1)", "comments (2)", 1)
		},
		"duplicate fingerprint": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = hfOutsideCategory(2, hfOutside(1, "engine/events.go", "19", "First."), hfOutside(1, "engine/events.go", "20", "Second.")) + "\n" + publicationTestMarker(1, 1)
		},
		"unknown summary tag": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.ReplaceAll(strings.ReplaceAll(f.snapshot.Reviews[0].Body, "<strong>", "<b>"), "</strong>", "</b>")
		},
		"mismatched summary tag": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "</strong>", "</em>", 1)
		},
		"unsupported category":    func(f *hfFixture) { f.snapshot.Reviews[0].Body += "\nUnknown inventory follows." },
		"partial source":          func(f *hfFixture) { f.snapshot.Complete = false },
		"unknown root collection": func(f *hfFixture) { f.snapshot.ReviewComments = nil },
		"missing batch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
		},
		"multiple complete batches": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
			r := f.snapshot.Reviews[0]
			r.ID++
			r.Body = strings.Replace(r.Body, "batch=1/2", "batch=2/2", 1)
			f.snapshot.Reviews = append(f.snapshot.Reviews, r)
		},
		"foreign actor":      func(f *hfFixture) { f.snapshot.Reviews[0].Author.ID++ },
		"foreign head":       func(f *hfFixture) { f.snapshot.Reviews[0].HeadSHA = strings.Repeat("b", 40) },
		"same second review": func(f *hfFixture) { f.snapshot.Reviews[0].SubmittedAt = f.snapshot.IssueComments[0].CreatedAt },
		"later command": func(f *hfFixture) {
			c := f.snapshot.IssueComments[0]
			c.ID++
			c.Body = "@coderabbitai review"
			c.CreatedAt = c.CreatedAt.Add(time.Second / 2)
			c.UpdatedAt = c.CreatedAt
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, c)
		},
		"edited trigger": func(f *hfFixture) {
			f.snapshot.IssueComments[0].UpdatedAt = f.snapshot.IssueComments[0].UpdatedAt.Add(time.Second)
		},
	}
	for _, orphan := range []bool{false, true} {
		name := "attached root"
		if orphan {
			name = "orphan root"
		}
		tests[name] = func(f *hfFixture) {
			root := ReviewComment{ID: 501, ReviewID: 401, Body: hfInline(2), Author: f.bot, Path: "engine/events.go", HeadSHA: f.intent.HeadSHA, OriginalHeadSHA: f.intent.HeadSHA, CreatedAt: f.snapshot.Reviews[0].SubmittedAt, UpdatedAt: f.snapshot.Reviews[0].SubmittedAt}
			if orphan {
				root.ReviewID = 499
			}
			f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, root)
		}
	}
	tests["old orphan edited in window"] = func(f *hfFixture) {
		at := f.snapshot.IssueComments[0].CreatedAt
		f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, ReviewComment{ID: 501, ReviewID: 499, Body: hfInline(2), Author: f.bot, Path: "engine/events.go", HeadSHA: f.intent.HeadSHA, OriginalHeadSHA: f.intent.HeadSHA, CreatedAt: at.Add(-time.Second), UpdatedAt: at.Add(time.Second)})
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f := outsideOnlyFixture(t, true)
			change(&f)
			before, _ := json.Marshal(f.snapshot)
			got := f.extract(t)
			if got.Complete || got.Publication.State == "attributed" {
				t.Fatalf("unknown inventory credited: %s/%s", got.Publication.State, got.Reason)
			}
			after, _ := json.Marshal(f.snapshot)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejection rewrote raw source")
			}
		})
	}
}

func TestHostedOutsideOnlyExplicitCountStillControlsInlineRoots(t *testing.T) {
	for _, count := range []string{"0", "1"} {
		t.Run(count, func(t *testing.T) {
			f := outsideOnlyFixture(t, true)
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: " + count + "**\n" + f.snapshot.Reviews[0].Body
			got := f.extract(t)
			if count == "0" {
				if !got.Complete || len(got.Findings) != 1 {
					t.Fatal("explicit zero-inline path regressed")
				}
			} else if got.Complete || got.Reason != "root_count_mismatch" {
				t.Fatal("explicit count was bypassed")
			}
		})
	}
}

func TestHostedOutsideOnlyHistoricalOrUnrelatedRootsDoNotCount(t *testing.T) {
	for _, kind := range []string{"old-provider-root", "other-actor-root", "reply"} {
		t.Run(kind, func(t *testing.T) {
			f := outsideOnlyFixture(t, true)
			at := f.snapshot.IssueComments[0].CreatedAt
			root := ReviewComment{ID: 501, ReviewID: 499, Body: hfInline(2), Author: f.bot, Path: "engine/old.go", HeadSHA: f.intent.HeadSHA, OriginalHeadSHA: f.intent.HeadSHA, CreatedAt: at.Add(time.Second), UpdatedAt: at.Add(time.Second)}
			switch kind {
			case "old-provider-root":
				root.CreatedAt = at.Add(-time.Second)
				root.UpdatedAt = root.CreatedAt
			case "other-actor-root":
				root.Author = f.poster
			case "reply":
				root.InReplyToID = 502
			}
			f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, root)
			got := f.extract(t)
			if !got.Complete || len(got.Findings) != 1 || got.Findings[0].Kind != "outside_diff" {
				t.Fatal("unrelated history changed current outside-only inventory")
			}
		})
	}
}
