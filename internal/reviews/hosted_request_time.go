package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

func HostedRequestTimeSchemaSQL() []string {
	return []string{`CREATE TABLE review_hosted_request_times (
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 run_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL CHECK(pull_number>0),
 head_sha TEXT NOT NULL,
 source_run_id TEXT NOT NULL,
 requested_at TEXT NOT NULL,
 PRIMARY KEY(project_id,run_id)
)`}
}

type requestTimeViewKey struct{}

func displayTiming(r *http.Request) bool {
	return r.Header.Get(displayStartHeader) == "v1" || r.Header.Get(displayStartHeader) == "v2"
}

func displayTimingContext(ctx context.Context, r *http.Request) context.Context {
	if r.Header.Get(displayStartHeader) == "v2" {
		return context.WithValue(ctx, requestTimeViewKey{}, true)
	}
	return ctx
}

// Record the request attached to the independently decoded original capture.
// It is a display annotation. The archive row and its chronological position
// remain bound to the provider publication.
func RecordExternalHostedRequestTime(ctx context.Context, tx *sql.Tx, project string, repository, pull int64, head, source, requestedAt, completedAt string) (string, error) {
	id := ExternalHostedRunID(project, source)
	detail, err := externalHostedArchiveRun(ctx, tx, project, repository, pull, id)
	if err != nil {
		return "", err
	}
	request, requestErr := time.Parse(time.RFC3339Nano, requestedAt)
	publication, publicationErr := time.Parse(time.RFC3339Nano, detail.Run.FinishedAt)
	completed, completedErr := time.Parse(time.RFC3339Nano, completedAt)
	if detail.Run.HeadSHA != head || detail.Run.SourceRunID != source ||
		requestErr != nil || publicationErr != nil || completedErr != nil ||
		!request.Before(publication) || publication.After(completed) {
		return "", nativeAdjudicationConflict()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_hosted_request_times(project_id,run_id,pull_number,head_sha,source_run_id,requested_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(project_id,run_id) DO NOTHING`, project, id, pull, head, source, requestedAt); err != nil {
		return "", err
	}
	var number int64
	var storedHead, storedSource, storedTime string
	if err = tx.QueryRowContext(ctx, `SELECT pull_number,head_sha,source_run_id,requested_at FROM review_hosted_request_times WHERE project_id=? AND run_id=?`, project, id).Scan(&number, &storedHead, &storedSource, &storedTime); err != nil {
		return "", err
	}
	if number != pull || storedHead != head || storedSource != source || storedTime != requestedAt {
		return "", nativeAdjudicationConflict()
	}
	return id, nil
}

func attachHostedRequestTimes(ctx context.Context, tx *sql.Tx, project string, groups ...[]RunSummary) error {
	if ctx.Value(requestTimeViewKey{}) != true {
		return nil
	}
	runs := make(map[string][]*RunSummary)
	ids := make([]string, 0)
	for _, group := range groups {
		for i := range group {
			run := &group[i]
			if run.Source != externalHostedSource || run.Channel != "hosted" || run.Outcome != "completed" {
				continue
			}
			if _, seen := runs[run.ID]; !seen {
				ids = append(ids, run.ID)
			}
			runs[run.ID] = append(runs[run.ID], run)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	selected, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.run_id,t.pull_number,t.head_sha,t.source_run_id,t.requested_at
FROM json_each(?) selected JOIN review_hosted_request_times t ON t.project_id=? AND t.run_id=selected.value`, string(selected), project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, head, source, requestedAt string
		var number int64
		if err := rows.Scan(&id, &number, &head, &source, &requestedAt); err != nil {
			return err
		}
		requested, err := time.Parse(time.RFC3339Nano, requestedAt)
		if err != nil || requested.IsZero() {
			continue
		}
		for _, run := range runs[id] {
			if run.PullNumber == number && run.HeadSHA == head && run.SourceRunID == source {
				if finish, err := time.Parse(time.RFC3339Nano, run.FinishedAt); err == nil && requested.Before(finish) {
					run.DisplayRequestAt = requestedAt
				}
			}
		}
	}
	return rows.Err()
}
