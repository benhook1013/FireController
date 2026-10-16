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
}

func (e *Error) Error() string { return e.Message }

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
	Title        string
	Number       int64
	RepositoryID int64
	State        string
	Merged       bool
	HeadSHA      string
	BaseSHA      string
	BaseRef      string
	ChangedFiles int64
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
	ID              int64
	ReviewID        int64
	Body            string
	Author          Author
	Path            string
	Line            *int64
	HeadSHA         string
	OriginalHeadSHA string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
