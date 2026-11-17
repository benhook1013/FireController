package reviewprovider

import (
	"strings"
	"testing"
)

func TestHostedOutsideInlinePreCodeRemainsVisible(t *testing.T) {
	for _, title := range []string{"Count text inside `<pre>` regions."} {
		t.Run(title, func(t *testing.T) {
			f := hfNew(t, []string{hfInline(1)}, hfOutsideCategory(1, hfOutside(2, "engine/queue.go", "12-15", title)))
			g := f.extract(t)
			if !g.Complete || len(g.Findings) != 2 || g.Findings[1].Title != title {
				t.Fatalf("lost literal pre title: %+v", g)
			}
		})
	}
}
func TestHostedInlinePreProjectionPreservesRealAndUnprovenTags(t *testing.T) {
	for _, body := range []string{"<pre>", "` unmatched <pre>", "\\`<pre>\\`", "`literal` <pre>"} {
		if !strings.Contains(findingPreStructuralText(body), "<pre>") {
			t.Fatal("hid actual/unproven tag", body)
		}
	}
}

func TestHostedInlinePreCodeSpanWidths(t *testing.T) {
	for _, text := range []string{"Count ``<pre>`` safely.", "Count ``a ` <pre> b`` safely."} {
		if strings.Contains(findingPreStructuralText(text), "<pre>") {
			t.Fatal("literal pre promoted", text)
		}
	}
}
