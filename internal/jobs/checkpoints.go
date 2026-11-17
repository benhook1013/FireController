package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

const (
	maxCheckpointText         = 8 << 10
	maxCheckpointBlocker      = 4 << 10
	maxCheckpointEvidence     = 10
	maxCheckpointEvidenceItem = 1024
	maxCheckpointAggregate    = 16 << 10
)

type checkpointRequest struct {
	ExpectedJobRevision int64    `json:"expected_job_revision"`
	Done                string   `json:"done"`
	Next                string   `json:"next"`
	Blocker             *string  `json:"blocker"`
	Evidence            []string `json:"evidence"`
}

type checkpointReceipt struct {
	ID          string `json:"id"`
	Sequence    int64  `json:"sequence"`
	JobRevision int64  `json:"job_revision"`
	Outcome     string `json:"outcome"`
}

type fullCheckpoint struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id"`
	JobRevision int64    `json:"job_revision"`
	Sequence    int64    `json:"sequence"`
	ActorID     string   `json:"actor_id"`
	WorkerID    string   `json:"worker_id"`
	Done        string   `json:"done"`
	Next        string   `json:"next"`
	Blocker     string   `json:"blocker"`
	Evidence    []string `json:"evidence"`
	CreatedAt   string   `json:"created_at"`
}

type latestCheckpointResponse struct {
	JobID       string          `json:"job_id"`
	JobRevision int64           `json:"job_revision"`
	Checkpoint  *fullCheckpoint `json:"checkpoint"`
}

type checkpointPage struct {
	Items   []fullCheckpoint `json:"items"`
	Cursor  string           `json:"cursor,omitempty"`
	HasMore bool             `json:"has_more"`
}

func latestCheckpoint(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		jobID := r.PathValue("job_id")
		response := latestCheckpointResponse{JobID: jobID}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			current, err := readJob(ctx, tx, scope.ProjectID, jobID)
			if err != nil {
				return err
			}
			response.JobRevision = current.Revision
			item, err := scanLatestCheckpoint(ctx, tx, scope.ProjectID, jobID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			response.Checkpoint = &item
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, response)
	}
}

func checkpointHistory(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "invalid_query", "limit", "cursor"); err != nil {
			core.WriteError(w, err)
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		jobID := r.PathValue("job_id")
		filter := "job-checkpoints:" + scope.ProjectID + ":" + jobID + ":sequence-desc"
		after, err := store.ParseCursor(scope, filter, r.URL.Query().Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var beforeSequence int64
		if after != "" {
			beforeSequence, err = strconv.ParseInt(after, 10, 64)
			if err != nil || beforeSequence < 1 {
				core.WriteError(w, core.Fail("validation_failed", "Use a valid cursor from this checkpoint history.", http.StatusBadRequest))
				return
			}
		}
		response := checkpointPage{Items: make([]fullCheckpoint, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := readJob(ctx, tx, scope.ProjectID, jobID); err != nil {
				return err
			}
			query := `SELECT sequence,id,project_id,job_id,job_revision,actor_id,worker_id,done,next,blocker,evidence_json,created_at
				FROM job_checkpoints WHERE project_id=? AND job_id=?`
			args := []any{scope.ProjectID, jobID}
			if beforeSequence > 0 {
				query += ` AND sequence<?`
				args = append(args, beforeSequence)
			}
			query += ` ORDER BY sequence DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			var lastSequence int64
			for rows.Next() {
				item, err := scanCheckpoint(rows)
				if err != nil {
					return err
				}
				if len(response.Items) == limit {
					response.HasMore = true
					break
				}
				lastSequence = item.Sequence
				response.Items = append(response.Items, item)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if response.HasMore && lastSequence > 0 {
				response.Cursor = store.Cursor(scope, filter, strconv.FormatInt(lastSequence, 10))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, response)
	}
}

type checkpointScanner interface {
	Scan(...any) error
}

func scanLatestCheckpoint(ctx context.Context, tx *sql.Tx, projectID, jobID string) (fullCheckpoint, error) {
	row := tx.QueryRowContext(ctx, `SELECT sequence,id,project_id,job_id,job_revision,actor_id,worker_id,done,next,blocker,evidence_json,created_at
		FROM job_checkpoints WHERE project_id=? AND job_id=? ORDER BY sequence DESC LIMIT 1`, projectID, jobID)
	return scanCheckpoint(row)
}

func scanCheckpoint(scanner checkpointScanner) (fullCheckpoint, error) {
	var item fullCheckpoint
	var evidence string
	if err := scanner.Scan(&item.Sequence, &item.ID, &item.ProjectID, &item.JobID, &item.JobRevision, &item.ActorID,
		&item.WorkerID, &item.Done, &item.Next, &item.Blocker, &evidence, &item.CreatedAt); err != nil {
		return fullCheckpoint{}, err
	}
	if err := json.Unmarshal([]byte(evidence), &item.Evidence); err != nil || item.Evidence == nil {
		return fullCheckpoint{}, core.Fail("storage_unavailable", "Stored checkpoint evidence is unavailable.", http.StatusInternalServerError)
	}
	return item, nil
}

func validateCheckpoint(req checkpointRequest) error {
	if req.ExpectedJobRevision < 1 {
		return core.Fail("invalid_revision", "expected_job_revision must be at least 1", http.StatusBadRequest)
	}
	if !validCheckpointText(req.Done, maxCheckpointText, true) || !validCheckpointText(req.Next, maxCheckpointText, true) {
		return core.Fail("validation_failed", "done and next must be nonblank valid text no longer than 8192 bytes", http.StatusBadRequest)
	}
	aggregate := len(req.Done) + len(req.Next)
	if req.Blocker != nil {
		if !validCheckpointText(*req.Blocker, maxCheckpointBlocker, false) {
			return core.Fail("validation_failed", "blocker must be valid text no longer than 4096 bytes", http.StatusBadRequest)
		}
		aggregate += len(*req.Blocker)
	}
	if len(req.Evidence) > maxCheckpointEvidence {
		return core.Fail("validation_failed", "evidence must contain at most 10 references", http.StatusBadRequest)
	}
	for _, evidence := range req.Evidence {
		if !validCheckpointText(evidence, maxCheckpointEvidenceItem, true) {
			return core.Fail("validation_failed", "each evidence reference must be nonblank valid text no longer than 1024 bytes", http.StatusBadRequest)
		}
		aggregate += len(evidence)
	}
	if aggregate > maxCheckpointAggregate {
		return core.Fail("validation_failed", "checkpoint text exceeds the 16384-byte aggregate limit", http.StatusBadRequest)
	}
	return nil
}

func validCheckpointText(value string, max int, nonblank bool) bool {
	if len(value) > max || !utf8.ValidString(value) || nonblank && strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if r < 0x20 && r != '\t' && r != '\r' && r != '\n' || r == 0x7f {
			return false
		}
	}
	return true
}
