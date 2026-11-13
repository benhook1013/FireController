package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func additionalAnnotationBaselineFixture(t *testing.T) (HostedPublicationSnapshot, HostedPublicationSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	s, baseline, intent, poster, bot, earliest := annotationBaselineFixture(t)
	additional, _ := additionalOnlyFixture(t)
	current := additional.snapshot.Reviews[0]
	current.Author, current.HeadSHA, current.SubmittedAt = bot, intent.HeadSHA, s.IssueComments[0].CreatedAt.Add(2*time.Second)
	s.Reviews = append(s.Reviews, current)
	s.IssueComments = s.IssueComments[:1]
	return s, baseline, intent, poster, bot, earliest
}

func TestHostedAdditionalOnlyProvenOldAnnotations(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		s, baseline, intent, poster, bot, earliest := additionalAnnotationBaselineFixture(t)
		wantCount := 1
		if mixed {
			s.Reviews[1].Body = hfOutsideCategory(1, hfOutside(8, "source/other.go", "7", "Preserve the original evidence.")) + "\n\n" + s.Reviews[1].Body
			wantCount = 2
		}
		before, _ := json.Marshal(s)
		prior, _ := json.Marshal(baseline)
		strict, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
		if err != nil || strict.Complete || strict.Reason != "invalid_actionable_count" {
			t.Fatal("expected historical edit barrier", strict.Reason, err)
		}
		got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
		if err != nil || !got.Complete || len(got.Findings) != wantCount || len(got.Publication.Roots) != 0 || len(got.Publication.Batches) != 1 {
			t.Fatal("proven old annotations blocked additional inventory", mixed, got.Reason, err)
		}
		last := got.Findings[len(got.Findings)-1]
		if last.Kind != "additional" || last.ReviewID != 401 || last.CommentID != 0 || last.Severity != "info" || !strings.Contains(last.Detail, "Unverified finding") || !strings.Contains(s.Reviews[1].Body, last.Detail) {
			t.Fatal("additional source identity, literal body or qualification lost", last)
		}
		after, _ := json.Marshal(s)
		later, _ := json.Marshal(baseline)
		if string(before) != string(after) || string(prior) != string(later) {
			t.Fatal("projection rewrote retained input")
		}
	}
}

func TestHostedAdditionalAnnotationProjectionRefusesUnprovenInventory(t *testing.T) {
	for name, edit := range map[string]func(*HostedPublicationSnapshot, *HostedPublicationSnapshot){
		"incomplete baseline":       func(_, b *HostedPublicationSnapshot) { b.Complete = false },
		"missing baseline root":     func(_, b *HostedPublicationSnapshot) { b.ReviewComments = []ReviewComment{} },
		"changed historical review": func(s, _ *HostedPublicationSnapshot) { s.Reviews[0].Body += "Changed content." },
		"changed historical root":   func(s, _ *HostedPublicationSnapshot) { s.ReviewComments[0].Body += "Changed content." },
		"late annotation":           func(s, _ *HostedPublicationSnapshot) { s.ReviewComments[0].UpdatedAt = s.ObservedAt },
		"fresh orphan": func(s, _ *HostedPublicationSnapshot) {
			r := s.ReviewComments[0]
			r.ID, r.ReviewID = 900, 999
			r.CreatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
			s.ReviewComments = append(s.ReviewComments, r)
		},
		"malformed explicit count": func(s, _ *HostedPublicationSnapshot) {
			s.Reviews[1].Body = "**Actionable comments posted: unknown**\n" + s.Reviews[1].Body
		},
		"contradictory explicit count": func(s, _ *HostedPublicationSnapshot) {
			s.Reviews[1].Body = "**Actionable comments posted: 1**\n" + s.Reviews[1].Body
		},
		"wrong additional count": func(s, _ *HostedPublicationSnapshot) {
			s.Reviews[1].Body = strings.Replace(s.Reviews[1].Body, "Additional comments: 1", "Additional comments: 2", 1)
		},
		"empty inventory": func(s, _ *HostedPublicationSnapshot) { s.Reviews[1].Body = publicationTestMarker(1, 1) },
		"duplicate only": func(s, _ *HostedPublicationSnapshot) {
			s.Reviews[1].Body = hfDuplicateCategory(1, hfDuplicateGroup("source/one.go", 1, hfDuplicateItem(8, "7", "Only a duplicate."))) + "\n" + publicationTestMarker(1, 1)
		},
		"wrong current actor": func(s, _ *HostedPublicationSnapshot) { s.Reviews[1].Author.ID++ },
		"wrong current head":  func(s, _ *HostedPublicationSnapshot) { s.Reviews[1].HeadSHA = strings.Repeat("d", 40) },
		"later command": func(s, _ *HostedPublicationSnapshot) {
			c := s.IssueComments[0]
			c.ID++
			c.Body = "@coderabbitai full review"
			c.CreatedAt, c.UpdatedAt = c.CreatedAt.Add(time.Second), c.CreatedAt.Add(time.Second)
			s.IssueComments = append(s.IssueComments, c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, b, intent, poster, bot, earliest := additionalAnnotationBaselineFixture(t)
			edit(&s, &b)
			got, err := ExtractHostedFindingsWithBaseline(s, b, intent, poster, bot, earliest)
			if err != nil || got.Complete || len(got.Findings) != 0 {
				t.Fatal("unproven source gained inventory", got.Reason, err)
			}
		})
	}
}
