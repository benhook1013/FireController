package reviewprovider

import (
	"strings"
	"testing"
	"time"
)

// A command acknowledgement proves neither reviewed scope nor an empty finding
// inventory. In particular, an older zero-finding review cannot fill that gap.
func TestHostedFinishedAcknowledgementCannotCreateCleanCoverage(t *testing.T) {
	for _, oldReview := range []bool{false, true} {
		name := "without_publication"
		if oldReview {
			name = "with_older_clean_publication"
		}
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := publicationFixture(t)
			ack := IssueComment{
				ID: 3001, Author: bot,
				Body:      "<!-- This is an auto-generated reply by CodeRabbit -->\n<details>\n<summary>Action performed</summary>\n\nFull review finished.\n\n</details>",
				CreatedAt: s.IssueComments[0].CreatedAt.Add(time.Second),
				UpdatedAt: s.IssueComments[0].CreatedAt.Add(3 * time.Second),
			}
			s.IssueComments = append(s.IssueComments, ack)
			s.ReviewComments = nil
			if oldReview {
				s.Reviews[0].HeadSHA = strings.Repeat("b", 40)
				s.Reviews[0].SubmittedAt = s.IssueComments[0].CreatedAt.Add(-time.Minute)
				s.Reviews[0].Body = "**Actionable comments posted: 0**\n" + publicationTestMarker(1, 1)
			} else {
				s.Reviews = nil
			}
			got := resolvePublication(t, s, intent, poster, bot, earliest, "active", "publication_not_proven")
			if got.PublicationID != "" || !got.FinishedAt.IsZero() || len(got.Batches) != 0 || len(got.Roots) != 0 {
				t.Fatal("finished acknowledgement invented publication evidence")
			}
		})
	}
}
