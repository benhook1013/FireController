// Package reviewsize describes optional project sizing information. It has no
// admission, accounting, storage or provider authority.
package reviewsize

import (
	"fmt"
	"strings"
)

type Limits struct{ Suggested, Maximum int64 }

type Assessment struct {
	PullNumber          int64  `json:"pull_number"`
	SuggestedMaxPRFiles int64  `json:"suggested_max_pr_files,omitempty"`
	MaxReviewFiles      int64  `json:"max_review_files,omitempty"`
	Files               *int64 `json:"files,omitempty"`
	RemainingFiles      *int64 `json:"remaining_files,omitempty"`
	ExcessFiles         *int64 `json:"excess_files,omitempty"`
	CompletedReviews    *int64 `json:"completed_reviews,omitempty"`
	RetainedReviews     int64  `json:"retained_reviews,omitempty"`
	AboveSuggested      bool   `json:"above_suggested,omitempty"`
	Unavailable         bool   `json:"unavailable,omitempty"`
	Message             string `json:"message"`
}

func Assess(number int64, limits Limits, files, completed *int64, retained int64) *Assessment {
	if limits.Suggested == 0 && limits.Maximum == 0 {
		return nil
	}
	a := &Assessment{PullNumber: number, SuggestedMaxPRFiles: limits.Suggested, MaxReviewFiles: limits.Maximum, Files: files, CompletedReviews: completed, RetainedReviews: retained}
	if files != nil {
		a.AboveSuggested = limits.Suggested > 0 && *files > limits.Suggested
		if limits.Maximum > 0 {
			n := limits.Maximum - *files
			if n >= 0 {
				a.RemainingFiles = &n
			} else {
				n = -n
				a.ExcessFiles = &n
			}
		}
	}
	a.Message = Message(*a)
	return a
}

func Unavailable(number int64) *Assessment {
	a := &Assessment{PullNumber: number, Unavailable: true}
	a.Message = Message(*a)
	return a
}

func Message(a Assessment) string {
	if a.Unavailable {
		return "PR size information is unavailable."
	}
	parts := []string{}
	if a.Files == nil {
		parts = append(parts, "File count not yet observed")
	} else {
		parts = append(parts, fmt.Sprintf("%d files", *a.Files))
	}
	if a.SuggestedMaxPRFiles > 0 {
		parts = append(parts, fmt.Sprintf("suggested maximum PR size %d", a.SuggestedMaxPRFiles))
	}
	if a.MaxReviewFiles > 0 {
		parts = append(parts, fmt.Sprintf("maximum review size %d", a.MaxReviewFiles))
	}
	if a.RemainingFiles != nil {
		parts = append(parts, fmt.Sprintf("%d files remaining", *a.RemainingFiles))
	}
	if a.ExcessFiles != nil {
		parts = append(parts, fmt.Sprintf("maximum exceeded by %d files", *a.ExcessFiles))
	}
	text := strings.Join(parts, "; ") + "."
	if a.CompletedReviews != nil {
		if *a.CompletedReviews == 0 && a.RetainedReviews == 0 {
			text += " No completed reviews."
		} else {
			text += fmt.Sprintf(" %d completed reviews (historical).", *a.CompletedReviews)
		}
	}
	if a.AboveSuggested {
		if a.CompletedReviews != nil && *a.CompletedReviews == 0 && a.RetainedReviews == 0 {
			text += " Consider splitting before starting review."
		} else {
			text += " Leave space for further fixes."
		}
	}
	return text
}

func Valid(a Assessment, number int64) bool {
	if a.PullNumber != number || number < 1 || number > 2147483647 {
		return false
	}
	if a.Unavailable {
		return a.SuggestedMaxPRFiles == 0 && a.MaxReviewFiles == 0 && a.Files == nil && a.RemainingFiles == nil && a.ExcessFiles == nil && a.CompletedReviews == nil && a.RetainedReviews == 0 && !a.AboveSuggested && a.Message == Message(a)
	}
	if a.SuggestedMaxPRFiles < 0 || a.SuggestedMaxPRFiles > 100000 || a.MaxReviewFiles < 0 || a.MaxReviewFiles > 100000 || a.SuggestedMaxPRFiles == 0 && a.MaxReviewFiles == 0 || a.RetainedReviews < 0 || a.Files != nil && (*a.Files < 0 || *a.Files > 2147483647) || a.CompletedReviews != nil && (*a.CompletedReviews < 0 || *a.CompletedReviews > a.RetainedReviews) {
		return false
	}
	expected := Assess(number, Limits{a.SuggestedMaxPRFiles, a.MaxReviewFiles}, a.Files, a.CompletedReviews, a.RetainedReviews)
	equal := func(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	return a.AboveSuggested == expected.AboveSuggested && equal(a.RemainingFiles, expected.RemainingFiles) && equal(a.ExcessFiles, expected.ExcessFiles) && a.Message == expected.Message
}
