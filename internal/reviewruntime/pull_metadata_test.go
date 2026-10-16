package reviewruntime

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCLIDriverLongTitleRemainsSourceFactAndFitsMetadata(t *testing.T) {
	for _, title := range []string{strings.Repeat("界", 170), strings.Repeat("🔥", 256), strings.Repeat(" ", 550) + "Visible title"} {
		f := newCLIDriverFixture(t)
		pull := f.reader.pulls[2]
		pull.Title = title
		f.reader.pulls[2] = pull
		op, a, c := f.start(t, 2, 1, "long-title")
		if _, err := f.driver.Prepare(t.Context(), op, c, a); err != nil {
			t.Fatalf("long title prevented preparation: %v", err)
		}
		var stored string
		if err := f.store.DB().QueryRow(`SELECT p.title FROM review_pulls p JOIN review_workspace w ON w.project_id=p.project_id AND w.snapshot_id=p.snapshot_id WHERE p.project_id=? AND p.number=2`, f.project).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if len(stored) > 500 || !utf8.ValidString(stored) || strings.TrimSpace(stored) == "" || !strings.HasPrefix(strings.TrimSpace(title), stored) || f.reader.pulls[2].Title != title {
			t.Fatalf("source or display title changed incorrectly: %q", stored)
		}
	}
}
