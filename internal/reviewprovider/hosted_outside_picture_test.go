package reviewprovider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestHostedOutsidePictureRetainsFourInlineAndOneOutsideFinding(t *testing.T) {
	for _, wrapper := range []string{"em", "strong"} {
		for _, dark := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_dark_%t", wrapper, dark), func(t *testing.T) {
				item := hfOutside(5, "engine/queue.go", "12-15", "Preserve the original queue record.")
				header := "<" + wrapper + ">" + standingTestPicture(dark) + " Minor</" + wrapper + ">"
				item = strings.Replace(item, "<em>🟡 Minor</em>", header, 1)
				// The header's captured text must retain the existing severity
				// mapping even without a second body metadata declaration.
				item = strings.Replace(item, "> _📐 Maintainability & Code Quality_ | _🟡 Minor_\n", "", 1)
				f := hfNew(t, []string{hfInline(1), hfInline(2), hfInline(3), hfInline(4)}, hfOutsideCategory(1, item))
				before, _ := json.Marshal(f.snapshot)
				got := hfWant(t, f, true, 5)
				if got.Reason != "finding_inventory_complete" || len(got.Publication.Roots) != 4 || got.Publication.FinishedAt.IsZero() {
					t.Fatal("lost actual inline inventory or finished publication identity")
				}
				for i, finding := range got.Findings[:4] {
					if finding.Kind != "inline" || finding.ReviewID != 401 || finding.CommentID != int64(501+i) || finding.Detail != hfInline(i+1) || finding.SourceKey != fmt.Sprintf("hosted-comment:%d:%024x", 501+i, i+1) {
						t.Fatal("outside header changed original inline source", i)
					}
				}
				outside := got.Findings[4]
				if outside.Kind != "outside_diff" || outside.ReviewID != 401 || outside.CommentID != 0 || outside.Path != "engine/queue.go" || outside.Title != "Preserve the original queue record." || outside.Severity != "low" || outside.StartLine == nil || *outside.StartLine != 12 || outside.EndLine == nil || *outside.EndLine != 15 || outside.Fingerprint != fmt.Sprintf("%024x", 5) || outside.SourceKey != fmt.Sprintf("hosted-review:401:outside_diff:%024x", 5) {
					t.Fatal("outside finding lost exact identity/location/severity", outside.SourceKey)
				}
				body := f.snapshot.Reviews[0].Body
				start := strings.Index(body, "> <details>")
				end := strings.Index(body, "> "+hfMarker(5)) + len("> "+hfMarker(5))
				if outside.Detail != body[start:end] || !strings.Contains(outside.Detail, header) {
					t.Fatal("outside detail no longer preserves original decorated bytes")
				}
				after, _ := json.Marshal(f.snapshot)
				if string(before) != string(after) {
					t.Fatal("decoder changed original provider snapshot")
				}
			})
		}
	}
}

func TestHostedOutsidePictureKeepsUnknownIconsAndInventoryGuardsClosed(t *testing.T) {
	icon := standingTestPicture(false)
	base := strings.Replace(hfOutside(5, "engine/queue.go", "12-15", "Preserve the original queue record."), "<em>🟡 Minor</em>", "<strong>"+icon+" Minor</strong>", 1)
	for name, change := range map[string]func(string) string{
		"unclosed_picture": func(s string) string { return strings.Replace(s, "</picture>", "", 1) },
		"unknown_wrapper":  func(s string) string { return strings.Replace(s, icon, "<span>"+icon+"</span>", 1) },
		"repeated_picture": func(s string) string { return strings.Replace(s, icon, icon+icon, 1) },
		"wrong_scheme":     func(s string) string { return strings.Replace(s, "https://", "http://", 1) },
		"nonempty_alt":     func(s string) string { return strings.Replace(s, `alt=""`, `alt="Finding"`, 1) },
		"extra_attribute": func(s string) string {
			return strings.Replace(s, `align="absmiddle"`, `align="absmiddle" class="extra"`, 1)
		},
		"missing_path":  func(s string) string { return strings.Replace(s, "> `engine/queue.go:12-15`\n", "", 1) },
		"missing_title": func(s string) string { return strings.Replace(s, "> **Preserve the original queue record.**\n", "", 1) },
		"second_marker": func(s string) string {
			return strings.Replace(s, "> "+hfMarker(5), "> "+hfMarker(5)+"\n> "+hfMarker(6), 1)
		},
		"unbalanced_blockquote": func(s string) string { return strings.Replace(s, "</blockquote></details>", "</details>", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			f := hfNew(t, []string{hfInline(1), hfInline(2), hfInline(3), hfInline(4)}, hfOutsideCategory(1, change(base)))
			before, _ := json.Marshal(f.snapshot)
			got := f.extract(t)
			if got.Complete || got.Reason == "finding_inventory_complete" || got.Publication.State != "attributed" {
				t.Fatal("picture support bypassed unknown markup or source inventory guard", got.Reason)
			}
			after, _ := json.Marshal(f.snapshot)
			if string(before) != string(after) {
				t.Fatal("refusal changed original provider snapshot")
			}
		})
	}
	t.Run("outside_count_mismatch", func(t *testing.T) {
		f := hfNew(t, []string{hfInline(1), hfInline(2), hfInline(3), hfInline(4)}, hfOutsideCategory(2, base))
		got := f.extract(t)
		if got.Complete || got.Reason != "finding_count_mismatch" {
			t.Fatal("picture support weakened exact outside count", got.Reason)
		}
	})
}
