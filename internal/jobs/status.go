package jobs

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

// jobStatus is a bounded status aid, not the complete working context.
type jobStatus struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	WorkerID          string `json:"worker_id"`
	Status            string `json:"status"`
	Revision          int64  `json:"revision"`
	UpdatedAt         string `json:"updated_at"`
	ProgressSummary   string `json:"progress_summary"`
	Next              string `json:"next"`
	Blocker           string `json:"blocker"`
	ProgressTruncated bool   `json:"progress_truncated"`
	NextTruncated     bool   `json:"next_truncated"`
	BlockerTruncated  bool   `json:"blocker_truncated"`
	EvidenceCount     int    `json:"evidence_count"`
}

func statusPrefix(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit], true
}

func getJobStatus(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "invalid_query"); err != nil {
			core.WriteError(w, err)
			return
		}
		var out jobStatus
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			// Deliberately select no brief, archive, checklist or note data.
			err := tx.QueryRowContext(ctx, `SELECT id,title,COALESCE(worker_id,''),status,revision,updated_at FROM jobs WHERE project_id=? AND id=?`, scope.ProjectID, r.PathValue("job_id")).Scan(&out.ID, &out.Title, &out.WorkerID, &out.Status, &out.Revision, &out.UpdatedAt)
			if errors.Is(err, sql.ErrNoRows) {
				return core.Fail("not_found", "job was not found", http.StatusNotFound)
			}
			if err != nil {
				return err
			}
			if !validPrimaryID(out.ID) || out.WorkerID != "" && !validPrimaryID(out.WorkerID) || out.Revision < 1 || out.Title != strings.TrimSpace(out.Title) || !utf8.ValidString(out.Title) || validateContent(out.Title, "", out.Status) != nil || !contextTime(out.UpdatedAt) {
				return contextUnavailable()
			}
			p, _, err := readProgress(ctx, tx, scope.ProjectID, out.ID, out.Revision)
			if err != nil {
				return err
			}
			out.ProgressSummary, out.ProgressTruncated = statusPrefix(p.Summary, 1024)
			out.Next, out.NextTruncated = statusPrefix(p.Next, 512)
			out.Blocker, out.BlockerTruncated = statusPrefix(p.Blocker, 512)
			out.EvidenceCount = len(p.Evidence)
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, out, 16<<10)
	}
}
