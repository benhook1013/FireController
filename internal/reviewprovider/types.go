// Package reviewprovider contains bounded external adapters for native reviews.
// It has no storage, job, inbox or project-selection dependencies.
package reviewprovider

import "time"

// Error carries only safe diagnostics. OutcomeUnknown distinguishes a possible
// external side effect from a rejected request; callers retain its attempt.
type Error struct {
	Code              string
	Message           string
	Status            int
	RetryAfterSeconds int64
	OutcomeUnknown    bool
	cause             error
}

func (e *Error) Error() string { return e.Message }

// Unwrap retains only a safe caller-context sentinel, never a transport body or URL.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type Author struct {
	ID    int64
	Login string
	Kind  string
}

type Repository struct {
	ID            int64
	FullName      string
	DefaultBranch string
}

type Pull struct {
	Title            string
	Number           int64
	RepositoryID     int64
	State            string
	Merged           bool
	HeadSHA          string
	HeadRef          string `json:",omitempty"`
	BaseSHA          string
	BaseRef          string
	ChangedFiles     int64
	Additions        *int64 `json:",omitempty"`
	Deletions        *int64 `json:",omitempty"`
	IsDraft          *bool  `json:",omitempty"`
	MergedAt         string `json:",omitempty"`
	Mergeability     string `json:",omitempty"`
	MergeStateStatus string `json:",omitempty"`
}

type IssueComment struct {
	ID        int64
	Body      string
	Author    Author
	CreatedAt time.Time
	UpdatedAt time.Time
}

type HostedReview struct {
	ID          int64
	Body        string
	State       string
	HeadSHA     string
	Author      Author
	SubmittedAt time.Time
}

type ReviewComment struct {
	// InReplyToID is zero for an attached root; positive values identify replies.
	InReplyToID       int64
	ID                int64
	ReviewID          int64
	Body              string
	Author            Author
	Path              string
	Line              *int64
	StartLine         *int64
	OriginalLine      *int64
	OriginalStartLine *int64
	Side              string
	StartSide         string
	HeadSHA           string
	OriginalHeadSHA   string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Page contains exactly one selected provider response, not a complete history.
// NextPage is the validated adjacent page number, or zero for a terminal page.
// Callers own cross-page identity checks, durable continuation and completeness.
type Page[T any] struct {
	Items    []T
	NextPage int64
}
