package jobs

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

// Select the retained row directly; current context and unrelated history never
// enter this response. Missing source history is not synthesized from today.
func getJobRevision(store *core.Store) http.HandlerFunc {
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
		revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
		if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != r.PathValue("revision") {
			core.WriteError(w, core.Fail("invalid_query", "Use one positive retained job revision.", http.StatusBadRequest))
			return
		}
		var item historyEntry
		jobID := r.PathValue("job_id")
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var exists int
			if err := tx.QueryRowContext(ctx, "SELECT 1 FROM jobs WHERE project_id=? AND id=?", scope.ProjectID, jobID).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return core.Fail("not_found", "job was not found", http.StatusNotFound)
				}
				return err
			}
			var state sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT revision,actor_id,CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json END,created_at FROM job_history WHERE project_id=? AND job_id=? AND revision=?`, maxJobContext, scope.ProjectID, jobID, revision).Scan(&item.Revision, &item.ActorID, &state, &item.CreatedAt)
			if errors.Is(err, sql.ErrNoRows) {
				return core.Fail("not_found", "The requested retained job revision was not found.", http.StatusNotFound)
			}
			if err != nil {
				return err
			}
			item.State = []byte(state.String)
			if !state.Valid || !validStoredJobHistory(item, scope.ProjectID, jobID) {
				return jobHistoryUnavailable()
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, item, maxProgressHistory)
	}
}
