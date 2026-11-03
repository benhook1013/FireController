package reviewprovider

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Synthetic source grammar only: no retained project paths or provider prose.
func hfAdditionalItem(location, title string) string {
	return "`" + location + "`: **Trivial** · **Maintainability & Code Quality** · **Quick win**\n\n⚠️ **Unverified finding**\nVerification did not confirm this synthetic observation.\n\n**" + title + "**\n\nIndependent rationale.\n\n<details>\n<summary>♻️ Proposed change</summary>\n<dl>\n<dd>\n```diff\n+ **Example title, not another finding**\n+ `70-70`: **Major**\n" + hfMarker(999) + "\n```\n</dd>\n</dl>\n</details>"
}
func hfAdditionalGroup(path, location string, count int, items ...string) string {
	return "<details>\n<summary>" + path + "-" + location + ": " + strconv.Itoa(count) + "</summary><blockquote>\n<dl>\n<dd>\n\n" + strings.Join(items, "\n\n") + "\n\n</dd>\n</dl>\n</blockquote></details>"
}

func hfAdditionalCategory(count int, groups ...string) string {
	return "<details>\n<summary>🔇 Additional comments: " + strconv.Itoa(count) + "</summary><blockquote>\n<dl>\n<dd>\n\n" + strings.Join(groups, "\n\n") + "\n\n</dd>\n</dl>\n</blockquote></details>"
}
func TestHostedAdditionalOriginalSourceAndIdentity(t *testing.T) {
	item := hfAdditionalItem("24-24", "Preserve the optional annotation.")
	f := hfNew(t, []string{hfInline(1)}, hfAdditionalCategory(1, hfAdditionalGroup("source/sample-file.go", "24-24", 1, item)))
	got := hfWant(t, f, true, 2).Findings[1]
	if got.Kind != "additional" || got.CommentID != 0 || got.ReviewID != 401 || got.Path != "source/sample-file.go" || *got.StartLine != 24 || *got.EndLine != 24 || got.Severity != "info" || got.Title != "Preserve the optional annotation." || got.Detail != item || !strings.Contains(got.Detail, "Unverified finding") || len(got.Fingerprint) != 24 || got.SourceKey != "hosted-review:401:additional:"+got.Fingerprint {
		t.Fatalf("source or unverified qualification changed: %+v", got)
	}
	if again := hfWant(t, f, true, 2).Findings[1]; again.SourceKey != got.SourceKey {
		t.Fatal("unstable source identity")
	}
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Independent rationale.", "Changed rationale.", 1)
	if changed := hfWant(t, f, true, 2).Findings[1]; changed.SourceKey == got.SourceKey {
		t.Fatal("changed source bytes reused identity")
	}
}
func TestHostedAdditionalMultipleItemsAndGroups(t *testing.T) {
	first := hfAdditionalItem("4-5", "First observation.")
	second := hfAdditionalItem("8-9", "Second observation.")
	third := hfAdditionalItem("12-12", "Third observation.")
	f := hfNew(t, nil, hfAdditionalCategory(3, hfAdditionalGroup("source/one.go", "4-9", 2, first, second), hfAdditionalGroup("source/two.go", "12-12", 1, third)))
	got := hfWant(t, f, true, 3)
	for i, want := range []string{first, second, third} {
		if got.Findings[i].Detail != want {
			t.Fatalf("item %d span changed", i)
		}
	}
}
func TestHostedAdditionalRejectsMalformedAndUnprovenInventory(t *testing.T) {
	item := hfAdditionalItem("24-24", "Preserve this boundary.")
	group := hfAdditionalGroup("source/file.go", "24-24", 1, item)
	base := hfAdditionalCategory(1, group)
	cases := map[string]string{
		"total count":       strings.Replace(base, "Additional comments: 1", "Additional comments: 2", 1),
		"group count":       strings.Replace(base, "24-24: 1", "24-24: 2", 1),
		"range mismatch":    strings.Replace(base, "`24-24`", "`23-24`", 1),
		"reversed":          strings.Replace(base, "`24-24`", "`25-24`", 1),
		"invalid path":      strings.Replace(base, "source/file.go", "../file.go", 1),
		"zero range":        strings.Replace(base, "`24-24`", "`0-24`", 1),
		"overflow":          strings.Replace(base, "`24-24`", "`2147483648-2147483648`", 1),
		"missing title":     strings.Replace(base, "**Preserve this boundary.**", "No bold title.", 1),
		"unbalanced layout": strings.Replace(base, "</dd>\n</dl>", "</dl>\n</dd>", 1),
		"crossed layout":    strings.Replace(base, "</dd>\n</dl>\n</blockquote></details>", "</blockquote>\n</dd>\n</dl>\n</details>", 1),
		"duplicate item":    hfAdditionalCategory(2, hfAdditionalGroup("source/file.go", "24-24", 2, item, item)),
		"unknown header":    strings.Replace(base, "🔇 Additional comments", "Additional findings", 1),
		"fake quoted item":  hfAdditionalCategory(1, hfAdditionalGroup("source/file.go", "24-24", 1, "> "+strings.ReplaceAll(item, "\n", "\n> "))),
	}
	for name, summary := range cases {
		t.Run(name, func(t *testing.T) {
			got := hfNew(t, nil, summary).extract(t)
			if got.Complete {
				t.Fatal("malformed inventory credited", got.Reason)
			}
		})
	}
}

func additionalOnlyFixture(t *testing.T) (hfFixture, string) {
	t.Helper()
	icon := standingTestPicture(false)
	item := strings.Replace(hfAdditionalItem("24-26", "Preserve the `*Sample` annotation."), "**Trivial**", "**"+icon+" Trivial**", 1)
	item = strings.Replace(item, "⚠️ **Unverified finding**", icon+" **Unverified finding**", 1)
	body := hfAdditionalCategory(1, hfAdditionalGroup("source/sample-file.go", "24-26", 1, item))
	body = strings.Replace(body, "🔇 Additional comments:", icon+" Additional comments:", 1)
	f := hfNew(t, nil, body)
	f.snapshot.Reviews[0].Body = strings.TrimPrefix(f.snapshot.Reviews[0].Body, "**Actionable comments posted: 0**\n\n")
	f.snapshot.ReviewComments = []ReviewComment{}
	return f, item
}

func TestHostedAdditionalOnlyOmittedInlineCount(t *testing.T) {
	f, item := additionalOnlyFixture(t)
	before, _ := json.Marshal(f.snapshot)
	got := f.extract(t)
	if !got.Complete || got.Publication.State != "attributed" || got.Reason != "finding_inventory_complete" || len(got.Findings) != 1 || len(got.Publication.Roots) != 0 || len(got.Publication.Batches) != 1 {
		t.Fatalf("completed additional-only inventory blocked: state=%s reason=%s complete=%t findings=%d", got.Publication.State, got.Reason, got.Complete, len(got.Findings))
	}
	finding := got.Findings[0]
	if finding.Kind != "additional" || finding.ReviewID != 401 || finding.CommentID != 0 || finding.Path != "source/sample-file.go" || finding.StartLine == nil || *finding.StartLine != 24 || finding.EndLine == nil || *finding.EndLine != 26 || finding.Severity != "info" || finding.Title != "Preserve the `*Sample` annotation." || finding.Detail != item || !strings.Contains(finding.Detail, "Unverified finding") || finding.Fingerprint == "" {
		t.Fatal("additional source or unverified qualification changed", finding)
	}
	after, _ := json.Marshal(f.snapshot)
	if string(before) != string(after) || got.Publication.Batches[0].Body != f.snapshot.Reviews[0].Body {
		t.Fatal("summary-only proof rewrote original source")
	}
	if again := f.extract(t); len(again.Findings) != 1 || again.Findings[0].SourceKey != finding.SourceKey {
		t.Fatal("additional source identity changed on exact replay")
	}
}

func TestHostedSummaryOnlyOmittedCountKeepsAttributionAndInventoryGuards(t *testing.T) {
	for name, change := range map[string]func(*hfFixture){
		"bare finished": func(f *hfFixture) { f.snapshot.Reviews[0].Body = "Review complete.\n" + publicationTestMarker(1, 1) },
		"empty":         func(f *hfFixture) { f.snapshot.Reviews[0].Body = publicationTestMarker(1, 1) },
		"zero additional": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = hfAdditionalCategory(0) + "\n" + publicationTestMarker(1, 1)
		},
		"duplicate only": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = hfDuplicateCategory(1, hfDuplicateGroup("source/sample-file.go", 1, hfDuplicateItem(1, "24-26", "Duplicate source."))) + "\n" + publicationTestMarker(1, 1)
		},
		"missing category": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = hfAdditionalGroup("source/sample-file.go", "24-26", 1, hfAdditionalItem("24-26", "Missing category.")) + "\n" + publicationTestMarker(1, 1)
		},
		"category count mismatch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Additional comments: 1", "Additional comments: 2", 1)
		},
		"group count mismatch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "sample-file.go-24-26: 1", "sample-file.go-24-26: 2", 1)
		},
		"malformed layout": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "</dd>", "</dl>", 1)
		},
		"quoted": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "> " + strings.ReplaceAll(f.snapshot.Reviews[0].Body, "\n", "\n> ") + "\n" + publicationTestMarker(1, 1)
		},
		"foreign author": func(f *hfFixture) { f.snapshot.Reviews[0].Author.ID++ },
		"foreign head":   func(f *hfFixture) { f.snapshot.Reviews[0].HeadSHA = strings.Repeat("b", 40) },
		"partial source": func(f *hfFixture) { f.snapshot.Complete = false },
		"unknown roots":  func(f *hfFixture) { f.snapshot.ReviewComments = nil },
		"unsupported inline root": func(f *hfFixture) {
			other := hfNew(t, []string{"Unsupported root"}, "")
			f.snapshot.ReviewComments = other.snapshot.ReviewComments
		},
		"new orphan root": func(f *hfFixture) {
			other := hfNew(t, []string{hfInline(1)}, "")
			other.snapshot.ReviewComments[0].ReviewID = 999
			f.snapshot.ReviewComments = other.snapshot.ReviewComments
		},
		"contradictory inline count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: 1**\n" + f.snapshot.Reviews[0].Body
		},
		"multiple batches": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
			other := f.snapshot.Reviews[0]
			other.ID++
			other.Body = strings.Replace(other.Body, "batch=1/2", "batch=2/2", 1)
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := additionalOnlyFixture(t)
			change(&f)
			got := f.extract(t)
			if got.Complete || len(got.Findings) != 0 {
				t.Fatal("unproven omitted-count inventory became complete", got.Reason, got.Findings)
			}
		})
	}
}

func TestHostedSummaryOnlyMixedAdditionalAndOutside(t *testing.T) {
	f, _ := additionalOnlyFixture(t)
	f.snapshot.Reviews[0].Body = hfOutsideCategory(1, hfOutside(8, "source/other-file.go", "7", "Preserve the original evidence.")) + "\n\n" + f.snapshot.Reviews[0].Body
	got := hfWant(t, f, true, 2)
	if len(got.Publication.Roots) != 0 || got.Findings[0].Kind != "outside_diff" || got.Findings[1].Kind != "additional" {
		t.Fatal("supported summary-only categories lost original inventory", got)
	}
}

func TestHostedAdditionalTitleRequiresClosedCodeAndBoldEnvelope(t *testing.T) {
	for name, title := range map[string]string{
		"pointer outside code":                               "Preserve *Sample literally.",
		"unclosed pointer code":                              "Preserve `*Sample literally.",
		"repaired delimiters cannot quote external emphasis": "Preserve `Sample` *outside `Other`.",
		"empty code is not a pointer span":                   "Preserve ``*Sample`` literally.",
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := additionalOnlyFixture(t)
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Preserve the `*Sample` annotation.", title, 1)
			got := f.extract(t)
			if got.Complete || len(got.Findings) != 0 {
				t.Fatal("malformed title supplied a complete inventory", got.Reason, got.Findings)
			}
		})
	}
}

func TestHostedOmittedCountMixedInlineAndAdditional(t *testing.T) {
	f, _ := additionalOnlyFixture(t)
	other := hfNew(t, []string{hfInline(1)}, "")
	f.snapshot.ReviewComments = other.snapshot.ReviewComments
	got := hfWant(t, f, true, 2)
	if got.Findings[0].Kind != "inline" || got.Findings[1].Kind != "additional" {
		t.Fatal("mixed positive inventory lost original categories", got)
	}
}
