package reviewprovider

import (
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
