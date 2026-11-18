package reviewprovider

import (
	"strings"
	"testing"
)

func TestCLIReviewPreservesBoundedLongTitles(t *testing.T) {
	for _, title := range []string{strings.Repeat("x", 612), strings.Repeat("é", 2048)} {
		f := cliFindingEvent()
		f["title"] = title
		r, err := ParseCLIReview(cliJSONLines(t, cliContextEvent(), f, cliCompleteEvent(1)), 0, syntheticCLIContext)
		if err != nil || !r.Complete || r.Findings[0].Title != title {
			t.Fatal(r, err)
		}
	}
	f := cliFindingEvent()
	f["title"] = strings.Repeat("x", MaxFindingTitleBytes+1)
	requireCLIInvalid(t, cliJSONLines(t, cliContextEvent(), f, cliCompleteEvent(1)), 0, syntheticCLIContext)
}
