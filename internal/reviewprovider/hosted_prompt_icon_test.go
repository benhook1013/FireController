package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func TestHostedFindingPromptIconPreservesFullInventory(t *testing.T) {
	for _, dark := range []bool{false, true} {
		header := "<summary>" + standingTestPicture(dark) + " Prompt to fix review comments</summary>"
		prompt := "<details>\n" + header + "\n<dl><dd>\n```text\nRepeated instructions are not another finding.\n" + hfMarker(999) + "\n```\n</dd></dl>\n</details>"
		var roots []string
		for i := 1; i <= 6; i++ {
			roots = append(roots, hfInline(i))
		}
		f := hfNew(t, roots, prompt)
		before := append([]HostedReview(nil), f.snapshot.Reviews...)
		got := hfWant(t, f, true, 6)
		for i, finding := range got.Findings {
			if finding.Detail != roots[i] || finding.CommentID != int64(501+i) {
				t.Fatal("prompt changed finding bytes/identity")
			}
		}
		if !reflect.DeepEqual(before, f.snapshot.Reviews) {
			t.Fatal("parser changed raw provider summary")
		}
		// Only an opaque fenced example is ignored. Visible markers still hold.
		f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "```text\nRepeated instructions are not another finding.\n"+hfMarker(999)+"\n```", hfMarker(999), 1)
		if got = f.extract(t); got.Complete || got.Reason != "finding_format_unsupported" {
			t.Fatal("decorated prompt hid a visible finding", got)
		}
	}
}

func TestHostedFindingPromptIconRejectsUnknownShapes(t *testing.T) {
	header := "<summary>" + standingTestPicture(true) + " Prompt to fix review comments</summary>"
	for name, bad := range map[string]string{
		"other label":          strings.Replace(header, "Prompt to fix review comments", "Review findings", 1),
		"trailing text":        header + " More findings",
		"unknown attribute":    strings.Replace(header, `alt=""`, `alt="" title="extra"`, 1),
		"nonempty alternative": strings.Replace(header, `alt=""`, `alt="Hidden finding"`, 1),
		"unknown source":       strings.Replace(header, `(prefers-color-scheme: dark)`, `other`, 1),
		"extra picture":        strings.Replace(header, "</picture>", "</picture>"+standingTestPicture(false), 1),
		"broken picture":       strings.Replace(header, "</picture>", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := hfNew(t, []string{hfInline(1)}, "<details>\n"+bad+"\nUnclassified content.\n</details>")
			if got := f.extract(t); got.Complete {
				t.Fatal("unknown prompt became trusted metadata", got)
			}
		})
	}
}
