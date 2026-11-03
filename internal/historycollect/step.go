// Package historycollect acquires explicitly selected, untrusted provider facts.
// It does not apply imports or establish native review authority.
package historycollect

import (
	"context"
	"encoding/json"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const maxPage = 2147483647

// Selection is deliberately finite. More operations may select more history;
// this bound is not a retained project-history quota.
type Selection struct {
	PullNumbers []int64 `json:"pull_numbers"`
}

// Position identifies one resource in a single uninterrupted acquisition pass.
// A durable owner must restart the unfinished resource on interrupted resume;
// old page numbers do not describe an immutable provider snapshot.
type Position struct {
	PullIndex int    `json:"pull_index"`
	Resource  string `json:"resource"`
	Page      int64  `json:"page"`
}

type Capture struct {
	Version        int             `json:"version"`
	RepositoryID   int64           `json:"repository_id"`
	RepositoryName string          `json:"repository_name"`
	PullNumber     int64           `json:"pull_number"`
	Resource       string          `json:"resource"`
	Page           int64           `json:"page"`
	CollectedAt    string          `json:"collected_at"`
	Count          int             `json:"count"`
	Facts          json.RawMessage `json:"facts"`
	Next           *Position       `json:"next,omitempty"`
}

// Provider exposes reads only. Each Page call returns one bounded provider GET;
// it cannot follow arbitrary URLs or post reviews/checkpoints.
type Provider interface {
	Repository(context.Context, int64) (reviewprovider.Repository, error)
	Pull(context.Context, string, int64) (reviewprovider.Pull, error)
	ReviewsPage(context.Context, string, int64, int64) (reviewprovider.Page[reviewprovider.HostedReview], error)
	ReviewCommentsPage(context.Context, string, int64, int64) (reviewprovider.Page[reviewprovider.ReviewComment], error)
	IssueCommentsPage(context.Context, string, int64, int64) (reviewprovider.Page[reviewprovider.IssueComment], error)
}

func invalid() error {
	return core.Fail("validation_failed", "Select distinct PR numbers and a valid collection position.", 400)
}

// Normalize owns its slice, so callers cannot change queued selection by
// mutating their input after validation.
func Normalize(s Selection) (Selection, error) {
	if len(s.PullNumbers) < 1 || len(s.PullNumbers) > 100 {
		return Selection{}, invalid()
	}
	seen := make(map[int64]bool, len(s.PullNumbers))
	out := Selection{PullNumbers: append([]int64(nil), s.PullNumbers...)}
	for _, n := range out.PullNumbers {
		if n < 1 || n > maxPage || seen[n] {
			return Selection{}, invalid()
		}
		seen[n] = true
	}
	return out, nil
}

func Start() Position { return Position{Resource: "pull", Page: 1} }

func validPosition(s Selection, p Position) bool {
	if p.PullIndex < 0 || p.PullIndex >= len(s.PullNumbers) || p.Page < 1 || p.Page > maxPage {
		return false
	}
	switch p.Resource {
	case "pull":
		return p.Page == 1
	case "reviews", "review_comments", "issue_comments":
		return true
	default:
		return false
	}
}

// Step performs provider work without a storage dependency. Its owner persists
// the capture and Next atomically before acknowledging progress. These facts
// have no import, source-finalization or native-coverage side effect.
func Step(ctx context.Context, provider Provider, repositoryID int64, selection Selection, position Position) (Capture, error) {
	s, err := Normalize(selection)
	if err != nil || ctx == nil || provider == nil || repositoryID < 1 || repositoryID > 9007199254740991 || !validPosition(s, position) {
		return Capture{}, invalid()
	}
	if err := ctx.Err(); err != nil {
		return Capture{}, err
	}
	repository, err := provider.Repository(ctx, repositoryID)
	if err != nil {
		return Capture{}, err
	}
	if repository.ID != repositoryID || repository.FullName == "" {
		return Capture{}, core.Fail("collection_source_changed", "The selected repository identity changed.", 409)
	}
	if err := ctx.Err(); err != nil {
		return Capture{}, err
	}
	number := s.PullNumbers[position.PullIndex]
	var facts any
	var count int
	var nextPage int64
	switch position.Resource {
	case "pull":
		var pull reviewprovider.Pull
		pull, err = provider.Pull(ctx, repository.FullName, number)
		if err == nil && (pull.RepositoryID != repositoryID || pull.Number != number) {
			return Capture{}, core.Fail("collection_source_changed", "The selected PR identity changed.", 409)
		}
		facts, count = pull, 1
	case "reviews":
		var page reviewprovider.Page[reviewprovider.HostedReview]
		page, err = provider.ReviewsPage(ctx, repository.FullName, number, position.Page)
		facts, count, nextPage = page.Items, len(page.Items), page.NextPage
	case "review_comments":
		var page reviewprovider.Page[reviewprovider.ReviewComment]
		page, err = provider.ReviewCommentsPage(ctx, repository.FullName, number, position.Page)
		facts, count, nextPage = page.Items, len(page.Items), page.NextPage
	case "issue_comments":
		var page reviewprovider.Page[reviewprovider.IssueComment]
		page, err = provider.IssueCommentsPage(ctx, repository.FullName, number, position.Page)
		facts, count, nextPage = page.Items, len(page.Items), page.NextPage
	}
	if err != nil {
		return Capture{}, err
	}
	if err := ctx.Err(); err != nil {
		return Capture{}, err
	}
	if nextPage != 0 && (position.Resource == "pull" || position.Page == maxPage || nextPage != position.Page+1) {
		return Capture{}, core.Fail("collection_source_invalid", "The provider collection continuation is invalid.", 502)
	}
	body, err := json.Marshal(facts)
	if err != nil || len(body) > 8<<20 || count > 100 || (position.Resource != "pull" && string(body) == "null") {
		return Capture{}, core.Fail("collection_source_invalid", "The provider collection page is invalid or too large.", 502)
	}
	next := position
	if nextPage != 0 {
		next.Page = nextPage
	} else {
		next.Page = 1
		switch position.Resource {
		case "pull":
			next.Resource = "reviews"
		case "reviews":
			next.Resource = "review_comments"
		case "review_comments":
			next.Resource = "issue_comments"
		case "issue_comments":
			next.PullIndex++
			next.Resource = "pull"
		}
	}
	result := Capture{Version: 1, RepositoryID: repositoryID, RepositoryName: repository.FullName, PullNumber: number, Resource: position.Resource, Page: position.Page, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano), Count: count, Facts: body}
	if next.PullIndex < len(s.PullNumbers) {
		result.Next = &next
	}
	return result, nil
}
