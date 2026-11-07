package reviewexec

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// OperationListOptions selects one page of retained local operations.
type OperationListOptions struct {
	PullNumber int64
	Channel    string
	Status     string
	Limit      int
	Cursor     string
}

type OperationSummary struct {
	ID         string `json:"id"`
	PullNumber int64  `json:"pull_number"`
	Channel    string `json:"channel"`
	HeadSHA    string `json:"head_sha"`
	WorkerID   string `json:"worker_id"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	AttemptID  string `json:"attempt_id"`
	Revision   int64  `json:"revision"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type OperationsPage struct {
	ProjectID string             `json:"project_id"`
	Items     []OperationSummary `json:"items"`
	Cursor    string             `json:"cursor,omitempty"`
	HasMore   bool               `json:"has_more"`
}

func operationListTime(value string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, value)
	return at, err == nil && !at.IsZero() && len(value) <= 40
}

func validOperationSummary(o OperationSummary) bool {
	_, c := operationListTime(o.CreatedAt)
	_, u := operationListTime(o.UpdatedAt)
	if !hostedObservationID(o.ID, "review_operation_") || !validRoutePull(o.PullNumber) || !validChannel(o.Channel) || !sha.MatchString(o.HeadSHA) || o.WorkerID != "" && !identifier.MatchString(o.WorkerID) || o.Reason != "" && !identifier.MatchString(o.Reason) || o.AttemptID != "" && !hostedObservationID(o.AttemptID, "review_attempt_") || o.Revision < 1 || !c || !u {
		return false
	}
	switch o.State {
	case "queued", "preparing", "posting", "active", "uncertain", "held", "completed", "failed", "cancelled", "rate_limited":
		return true
	}
	return false
}

// ListOperations reads one authorized page. Mutable states are evaluated anew
// on every page; a cursor does not promise a historical state snapshot.
func (e *Engine) ListOperations(ctx context.Context, scope core.Scope, options OperationListOptions) (OperationsPage, error) {
	if ctx == nil || e == nil || e.store == nil || options.PullNumber != 0 && !validRoutePull(options.PullNumber) || options.Channel != "" && !validChannel(options.Channel) || options.Limit < 0 || options.Limit > 100 {
		return OperationsPage{}, invalid()
	}
	if options.Status == "" {
		options.Status = "pending"
	}
	if options.Status != "pending" && options.Status != "all" {
		return OperationsPage{}, invalid()
	}
	if options.Limit == 0 {
		options.Limit = 5
	}
	filter := "reviews.operations:v1:" + strconv.FormatInt(options.PullNumber, 10) + ":" + options.Channel + ":" + options.Status
	position, err := e.store.ParseCursor(scope, filter, options.Cursor)
	if err != nil {
		return OperationsPage{}, err
	}
	at, id := "", ""
	if position != "" {
		at, id, _ = strings.Cut(position, "|")
		if _, ok := operationListTime(at); !ok || !hostedObservationID(id, "review_operation_") {
			return OperationsPage{}, invalid()
		}
	}
	result := OperationsPage{ProjectID: scope.ProjectID, Items: []OperationSummary{}}
	err = e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		query := `SELECT o.id,o.pull_number,o.channel,o.head_sha,o.worker_id,o.state,o.reason,o.attempt_id,o.revision,o.created_at,o.updated_at FROM review_operations o WHERE o.project_id=?`
		args := []any{scope.ProjectID}
		if options.PullNumber != 0 {
			query += ` AND o.pull_number=?`
			args = append(args, options.PullNumber)
		}
		if options.Channel != "" {
			query += ` AND o.channel=?`
			args = append(args, options.Channel)
		}
		if options.Status == "pending" {
			query += ` AND (o.state IN ('queued','preparing','posting','active','uncertain') OR (o.state='held' AND EXISTS (SELECT 1 FROM review_execution_reservations r JOIN review_execution_attempts a ON a.id=r.attempt_id WHERE a.id=o.attempt_id AND a.operation_id=o.id AND a.project_id=o.project_id)))`
		}
		if position != "" {
			query += ` AND (o.created_at,o.id)<(?,?)`
			args = append(args, at, id)
		}
		query += ` ORDER BY o.created_at DESC,o.id DESC LIMIT ?`
		args = append(args, options.Limit+1)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item OperationSummary
			if err := rows.Scan(&item.ID, &item.PullNumber, &item.Channel, &item.HeadSHA, &item.WorkerID, &item.State, &item.Reason, &item.AttemptID, &item.Revision, &item.CreatedAt, &item.UpdatedAt); err != nil {
				return err
			}
			if !validOperationSummary(item) {
				return core.Fail("storage_unavailable", "Stored review operation summaries are unavailable.", 500)
			}
			if len(result.Items) == options.Limit {
				result.HasMore = true
				break
			}
			result.Items = append(result.Items, item)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if result.HasMore {
			last := result.Items[len(result.Items)-1]
			result.Cursor = e.store.Cursor(scope, filter, last.CreatedAt+"|"+last.ID)
		}
		return nil
	})
	if err != nil {
		return OperationsPage{}, err
	}
	return result, nil
}
