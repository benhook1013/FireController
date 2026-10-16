package reviewruntime

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHostedDriverLongTitleFitsDisplayWithoutChangingSource(t *testing.T) {
	for _, title := range []string{strings.Repeat("界", 170), strings.Repeat("🔥", 256)} {
		f := newHostedDriverFixture(t)
		pull := f.reader.pulls[2]
		pull.Title = title
		f.reader.pulls[2] = pull
		f.prepareHosted(t)
		var stored string
		if err := f.store.DB().QueryRow(`SELECT p.title FROM review_pulls p JOIN review_workspace w ON w.project_id=p.project_id AND w.snapshot_id=p.snapshot_id WHERE p.project_id=? AND p.number=2`, f.project).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if len(stored) > 500 || !utf8.ValidString(stored) || !strings.HasPrefix(title, stored) || stored == "" || f.reader.pulls[2].Title != title || f.provider.reviews != 0 {
			t.Fatal("hosted preparation changed source title or failed display budget", stored)
		}
	}
}
