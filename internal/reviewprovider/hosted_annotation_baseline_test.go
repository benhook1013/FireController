package reviewprovider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func annotationBaselineFixture(t *testing.T) (HostedPublicationSnapshot, HostedPublicationSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	s, intent, poster, bot, earliest := standingTestFixture(t)
	trigger := s.IssueComments[0].CreatedAt
	oldHead := strings.Repeat("a", 40)
	review := HostedReview{ID: 801, Body: "Original synthetic review summary.", State: "commented", HeadSHA: oldHead, Author: bot, SubmittedAt: trigger.Add(-time.Hour)}
	line := int64(11)
	root := ReviewComment{ID: 802, ReviewID: review.ID, Body: hfInline(71), Author: bot, Path: "engine/prior.go", Line: &line, OriginalLine: &line, Side: "RIGHT", HeadSHA: oldHead, OriginalHeadSHA: oldHead, CreatedAt: trigger.Add(-time.Hour), UpdatedAt: trigger.Add(-time.Minute)}
	baseline := HostedPublicationSnapshot{Complete: true, ObservedAt: trigger.Add(-time.Second), IssueComments: []IssueComment{}, Reviews: []HostedReview{review}, ReviewComments: []ReviewComment{root}}
	s.Reviews = []HostedReview{review}
	root.Body += "\n\n✅ Addressed in commit " + intent.HeadSHA[:7]
	root.UpdatedAt = trigger.Add(time.Second)
	s.ReviewComments = []ReviewComment{root}
	return s, baseline, intent, poster, bot, earliest
}

func TestHostedAnnotationBaselineCompletion(t *testing.T) {
	s, baseline, intent, poster, bot, earliest := annotationBaselineFixture(t)
	strict, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
	if err != nil || strict.Complete {
		t.Fatal("fixture must preserve strict edited-root refusal", strict, err)
	}
	got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
	if err != nil || !got.Complete || got.Publication.StandingZero == nil {
		t.Fatalf("canonical annotation prevented standing zero: complete=%v reason=%s err=%v", got.Complete, got.Reason, err)
	}
}

func TestHostedAnnotationBaselineRejectsUnprovenChanges(t *testing.T) {
	changes := map[string]func(*HostedPublicationSnapshot, *HostedPublicationSnapshot){
		"incomplete baseline":     func(_, b *HostedPublicationSnapshot) { b.Complete = false },
		"nil baseline issues":     func(_, b *HostedPublicationSnapshot) { b.IssueComments = nil },
		"nil baseline reviews":    func(_, b *HostedPublicationSnapshot) { b.Reviews = nil },
		"nil baseline roots":      func(_, b *HostedPublicationSnapshot) { b.ReviewComments = nil },
		"missing baseline root":   func(_, b *HostedPublicationSnapshot) { b.ReviewComments = []ReviewComment{} },
		"missing baseline review": func(_, b *HostedPublicationSnapshot) { b.Reviews = []HostedReview{} },
		"missing current review":  func(s, _ *HostedPublicationSnapshot) { s.Reviews = []HostedReview{} },
		"duplicate baseline root": func(_, b *HostedPublicationSnapshot) {
			b.ReviewComments = append(b.ReviewComments, b.ReviewComments[0])
		},
		"duplicate baseline review": func(_, b *HostedPublicationSnapshot) { b.Reviews = append(b.Reviews, b.Reviews[0]) },
		"duplicate baseline issue": func(s, b *HostedPublicationSnapshot) {
			c := s.IssueComments[1]
			c.UpdatedAt = c.CreatedAt
			b.IssueComments = []IssueComment{c, c}
		},
		"equal observation":            func(s, b *HostedPublicationSnapshot) { b.ObservedAt = s.ObservedAt },
		"later observation":            func(s, b *HostedPublicationSnapshot) { b.ObservedAt = s.ObservedAt.Add(time.Second) },
		"invalid observation":          func(_, b *HostedPublicationSnapshot) { b.ObservedAt = time.Time{} },
		"baseline foreign actor":       func(_, b *HostedPublicationSnapshot) { b.ReviewComments[0].Author.ID++ },
		"baseline actor login changed": func(_, b *HostedPublicationSnapshot) { b.ReviewComments[0].Author.Login = "other-reviewer" },
		"nonroot": func(s, b *HostedPublicationSnapshot) {
			s.ReviewComments[0].InReplyToID = 800
			b.ReviewComments[0].InReplyToID = 800
		},
		"body rewritten": func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].Body = strings.Replace(s.ReviewComments[0].Body, "Retain", "Rewrite", 1)
		},
		"new prose": func(s, _ *HostedPublicationSnapshot) { s.ReviewComments[0].Body += "\nNew finding." },
		"unknown footer": func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].Body = strings.Replace(s.ReviewComments[0].Body, "Addressed", "Fixed", 1)
		},
		"wrong commit": func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].Body = strings.ReplaceAll(s.ReviewComments[0].Body, strings.Repeat("c", 7), strings.Repeat("d", 7))
		},
		"short commit": func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].Body = strings.TrimSuffix(s.ReviewComments[0].Body, "c")
		},
		"uppercase commit": func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].Body = strings.ReplaceAll(s.ReviewComments[0].Body, strings.Repeat("c", 7), strings.Repeat("C", 7))
		},
		"path moved":          func(s, _ *HostedPublicationSnapshot) { s.ReviewComments[0].Path = "engine/else.go" },
		"line moved":          func(s, _ *HostedPublicationSnapshot) { v := int64(12); s.ReviewComments[0].Line = &v },
		"head moved":          func(s, _ *HostedPublicationSnapshot) { s.ReviewComments[0].HeadSHA = strings.Repeat("e", 40) },
		"original head moved": func(s, _ *HostedPublicationSnapshot) { s.ReviewComments[0].OriginalHeadSHA = strings.Repeat("e", 40) },
		"creation moved": func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].CreatedAt = s.ReviewComments[0].CreatedAt.Add(time.Second)
		},
		"new root": func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].CreatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
		},
		"baseline edit at trigger": func(s, b *HostedPublicationSnapshot) {
			b.ObservedAt = s.IssueComments[0].CreatedAt
			b.ReviewComments[0].UpdatedAt = s.IssueComments[0].CreatedAt
		},
		"review body changed":     func(s, _ *HostedPublicationSnapshot) { s.Reviews[0].Body += "\nChanged finding." },
		"review identity changed": func(s, _ *HostedPublicationSnapshot) { s.Reviews[0].ID++ },
		"review actor changed":    func(_, b *HostedPublicationSnapshot) { b.Reviews[0].Author.ID++ },
		"review submitted changed": func(s, _ *HostedPublicationSnapshot) {
			s.Reviews[0].SubmittedAt = s.Reviews[0].SubmittedAt.Add(time.Second)
		},
		"review not historical": func(s, b *HostedPublicationSnapshot) {
			s.Reviews[0].SubmittedAt = s.IssueComments[0].CreatedAt
			b.Reviews[0] = s.Reviews[0]
			b.ObservedAt = s.IssueComments[0].CreatedAt
		},
		"review wrong original head": func(s, b *HostedPublicationSnapshot) {
			s.Reviews[0].HeadSHA = strings.Repeat("e", 40)
			b.Reviews[0] = s.Reviews[0]
		},
		"annotation after finish": func(s, _ *HostedPublicationSnapshot) { s.ReviewComments[0].UpdatedAt = s.ObservedAt },
		"no finished ack":         func(s, _ *HostedPublicationSnapshot) { s.IssueComments = s.IssueComments[:2] },
		"new provider finding": func(s, _ *HostedPublicationSnapshot) {
			r := s.ReviewComments[0]
			r.ID++
			r.Body = hfInline(72)
			r.CreatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
			s.ReviewComments = append(s.ReviewComments, r)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, b, intent, poster, bot, earliest := annotationBaselineFixture(t)
			change(&s, &b)
			want, wantErr := ExtractHostedFindings(s, intent, poster, bot, earliest)
			got, err := ExtractHostedFindingsWithBaseline(s, b, intent, poster, bot, earliest)
			if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(got, want) {
				t.Fatalf("unproven annotation changed strict result: got=%+v err=%v want=%+v err=%v", got, err, want, wantErr)
			}
		})
	}
}

func TestHostedAnnotationBaselineExactFootersAndRawConservation(t *testing.T) {
	for _, length := range []int{7, 12, 40} {
		for _, newline := range []string{"", "\n"} {
			s, b, intent, poster, bot, earliest := annotationBaselineFixture(t)
			s.ReviewComments[0].Body = b.ReviewComments[0].Body + "\n\n✅ Addressed in commit " + intent.HeadSHA[:length] + newline
			second := s.ReviewComments[0]
			second.ID++
			second.UpdatedAt = s.IssueComments[2].UpdatedAt // Equality at the exact finish is permitted.
			s.ReviewComments = append(s.ReviewComments, second)
			second = b.ReviewComments[0]
			second.ID++
			b.ReviewComments = append(b.ReviewComments, second)
			// Provider clocks truncate to seconds; local pre-POST ownership is
			// independently proven by the runtime, not this provider timestamp.
			b.ObservedAt = s.IssueComments[0].CreatedAt.Add(time.Millisecond)
			beforeCurrent, _ := json.Marshal(s)
			beforeBaseline, _ := json.Marshal(b)
			got, err := ExtractHostedFindingsWithBaseline(s, b, intent, poster, bot, earliest)
			if err != nil || !got.Complete || got.Publication.StandingZero == nil || len(got.Findings) != 0 || len(got.Publication.Roots) != 0 {
				t.Fatal("proven annotations did not yield only standing zero", got, err)
			}
			got.Publication.StandingZero.Summary.Body = "Return-value mutation cannot rewrite raw capture."
			afterCurrent, _ := json.Marshal(s)
			afterBaseline, _ := json.Marshal(b)
			if string(beforeCurrent) != string(afterCurrent) || string(beforeBaseline) != string(afterBaseline) {
				t.Fatal("attribution changed a raw snapshot or caller metadata")
			}
		}
	}
}

func TestHostedAnnotationBaselinePreservesStrictResults(t *testing.T) {
	for _, mode := range []string{"complete findings", "partial batch", "invalid intent", "complete zero", "invalid current collection"} {
		t.Run(mode, func(t *testing.T) {
			s, b, intent, poster, bot, earliest := annotationBaselineFixture(t)
			switch mode {
			case "complete findings":
				f := hfNew(t, []string{hfInline(1)}, "")
				s, intent, poster, bot, earliest = f.snapshot, f.intent, f.poster, f.bot, f.earliest
			case "partial batch":
				review := HostedReview{ID: 990, Body: "**Actionable comments posted: 0**\n\n" + publicationTestMarker(1, 2), State: "commented", HeadSHA: intent.HeadSHA, Author: bot, SubmittedAt: s.IssueComments[0].CreatedAt.Add(time.Second)}
				s.Reviews = append(s.Reviews, review)
			case "invalid intent":
				intent.HeadSHA = "invalid"
			case "complete zero":
				s.ReviewComments = []ReviewComment{}
				b = HostedPublicationSnapshot{} // Complete/default paths ignore the optional baseline.
			case "invalid current collection":
				s.ReviewComments = append(s.ReviewComments, s.ReviewComments[0])
			}
			want, wantErr := ExtractHostedFindings(s, intent, poster, bot, earliest)
			got, err := ExtractHostedFindingsWithBaseline(s, b, intent, poster, bot, earliest)
			if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(got, want) {
				t.Fatal("baseline replaced an existing strict result", got, err, want, wantErr)
			}
		})
	}
}
