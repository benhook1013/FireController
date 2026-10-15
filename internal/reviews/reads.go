package reviews

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

type snapshotMeta struct {
	ProjectID  string `json:"project_id"`
	Revision   int64  `json:"revision"`
	SnapshotID string `json:"snapshot_id"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at"`
	RecordedAt string `json:"recorded_at"`
}

func queue(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "validation_failed", "limit", "cursor")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if len(r.URL.RawQuery) > 4096 {
			core.WriteError(w, validationError("The query exceeds its bounded size."))
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "reviews.queue:v1"
		after, err := store.ParseCursor(scope, filter, query.Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		snapshotID, position, err := decodePosition(after)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		response := QueueResponse{ProjectID: scope.ProjectID, Items: make([]PullSummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, snapshotID)
			if err != nil {
				return err
			}
			if !found {
				if snapshotID != "" {
					return notFound("The review snapshot is unavailable.")
				}
				return nil
			}
			response.Configured = true
			setMetaQueue(&response, meta)
			rows, err := tx.QueryContext(ctx, `SELECT number,title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete,run_count,attempt_count,recent_runs_json,latest_attempts_json,position
				FROM review_pulls WHERE project_id=? AND snapshot_id=? AND position>? ORDER BY position LIMIT ?`,
				scope.ProjectID, meta.SnapshotID, position, limit+1)
			if err != nil {
				return err
			}
			defer rows.Close()
			lastPosition := -1
			for rows.Next() {
				var item PullSummary
				var complete int
				var recentJSON, latestJSON string
				var rowPosition int
				if err := rows.Scan(&item.Number, &item.Title, &item.State, &item.HeadSHA, &item.BaseRef, &item.BaseSHA, &item.CIState, &item.Mergeability, &complete, &item.RunCount, &item.AttemptCount, &recentJSON, &latestJSON, &rowPosition); err != nil {
					return err
				}
				if len(response.Items) == limit {
					response.HasMore = true
					break
				}
				item.Complete = complete != 0
				if err := strictStoredJSON(recentJSON, &item.RecentRuns); err != nil {
					return err
				}
				// Pre-mixed snapshots stored one producer in snapshot metadata.
				// Fill their old compact projection only in the read response.
				for i := range item.RecentRuns {
					if item.RecentRuns[i].Source == "" {
						item.RecentRuns[i].Source = meta.Source
					}
				}
				if err := strictStoredJSON(latestJSON, &item.LatestAttempts); err != nil {
					return err
				}
				response.Items = append(response.Items, item)
				lastPosition = rowPosition
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if response.HasMore && lastPosition >= 0 {
				response.Cursor = store.Cursor(scope, filter, encodePosition(meta.SnapshotID, lastPosition))
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

func pull(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "validation_failed", "snapshot")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if len(r.URL.RawQuery) > 4096 {
			core.WriteError(w, validationError("The query exceeds its bounded size."))
			return
		}
		number, err := parsePullNumber(r.PathValue("number"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		response := PullResponse{ProjectID: scope.ProjectID, Attempts: []AttemptSummary{}}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, query.Get("snapshot"))
			if err != nil {
				return err
			}
			if !found {
				return notFound("The review snapshot is unavailable.")
			}
			if err := setMetaPull(&response, meta); err != nil {
				return err
			}
			var complete int
			var recentJSON, latestJSON string
			err = tx.QueryRowContext(ctx, `SELECT number,title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete,run_count,attempt_count,recent_runs_json,latest_attempts_json FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?`, scope.ProjectID, meta.SnapshotID, number).Scan(
				&response.Pull.Number, &response.Pull.Title, &response.Pull.State, &response.Pull.HeadSHA, &response.Pull.BaseRef, &response.Pull.BaseSHA, &response.Pull.CIState, &response.Pull.Mergeability, &complete, &response.Pull.RunCount, &response.Pull.AttemptCount, &recentJSON, &latestJSON)
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("The pull request is unavailable in this snapshot.")
			}
			if err != nil {
				return err
			}
			response.Pull.Complete = complete != 0
			if err := strictStoredJSON(recentJSON, &response.Pull.RecentRuns); err != nil {
				return err
			}
			for i := range response.Pull.RecentRuns {
				if response.Pull.RecentRuns[i].Source == "" {
					response.Pull.RecentRuns[i].Source = meta.Source
				}
			}
			if err := strictStoredJSON(latestJSON, &response.Pull.LatestAttempts); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT source_attempt_id,channel,head_sha,state,started_at,finished_at,duration_seconds,source_run_id,trigger_id,provider_review_id FROM review_attempts WHERE project_id=? AND snapshot_id=? AND pull_number=? ORDER BY position`, scope.ProjectID, meta.SnapshotID, number)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item AttemptSummary
				if err := rows.Scan(&item.SourceAttemptID, &item.Channel, &item.HeadSHA, &item.State, &item.StartedAt, &item.FinishedAt, &item.DurationSeconds, &item.SourceRunID, &item.TriggerID, &item.ProviderReviewID); err != nil {
					return err
				}
				response.Attempts = append(response.Attempts, item)
			}
			return rows.Err()
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, response)
	}
}

func runs(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "validation_failed", "snapshot", "limit", "cursor")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if len(r.URL.RawQuery) > 4096 {
			core.WriteError(w, validationError("The query exceeds its bounded size."))
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		number, err := parsePullNumber(r.PathValue("number"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := fmt.Sprintf("reviews.runs:%d:v1", number)
		after, err := store.ParseCursor(scope, filter, query.Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		cursorSnapshot, cursorOrder, err := decodePosition(after)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		requested := query.Get("snapshot")
		if requested != "" && cursorSnapshot != "" && requested != cursorSnapshot {
			core.WriteError(w, validationError("The snapshot must match the runs cursor."))
			return
		}
		if requested == "" {
			requested = cursorSnapshot
		}
		response := RunsResponse{ProjectID: scope.ProjectID, Items: make([]RunSummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, requested)
			if err != nil {
				return err
			}
			if !found {
				return notFound("The review snapshot is unavailable.")
			}
			setMetaRuns(&response, meta)
			if _, err := pullExists(ctx, tx, scope.ProjectID, meta.SnapshotID, number); err != nil {
				return err
			}
			querySQL := `SELECT id,pull_number,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json,run_order FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=?`
			args := []any{scope.ProjectID, meta.SnapshotID, number}
			if cursorSnapshot != "" {
				querySQL += ` AND run_order<?`
				args = append(args, cursorOrder)
			}
			querySQL += ` ORDER BY run_order DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, querySQL, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			lastOrder := -1
			for rows.Next() {
				var item RunSummary
				var attributable, complete, finalized int
				var limits string
				var order int
				if err := rows.Scan(&item.ID, &item.PullNumber, &item.Source, &item.SourceRunID, &item.Channel, &item.HeadSHA, &item.Reviewer, &item.Scope, &item.Outcome, &attributable, &complete, &item.StartedAt, &item.FinishedAt, &item.Found, &item.Accepted, &item.Routed, &finalized, &item.Fixed, &limits, &order); err != nil {
					return err
				}
				if len(response.Items) == limit {
					response.HasMore = true
					break
				}
				item.Attributable = attributable != 0
				item.Complete = complete != 0
				item.Finalized = finalized != 0
				if err := strictStoredJSON(limits, &item.CoverageLimits); err != nil {
					return err
				}
				response.Items = append(response.Items, item)
				lastOrder = order
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if response.HasMore && lastOrder >= 0 {
				response.Cursor = store.Cursor(scope, filter, encodePosition(meta.SnapshotID, lastOrder))
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

func runDetail(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "validation_failed", "snapshot")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if len(r.URL.RawQuery) > 4096 {
			core.WriteError(w, validationError("The query exceeds its bounded size."))
			return
		}
		runID := r.PathValue("run_id")
		if len(runID) != len("review_run_")+32 || !strings.HasPrefix(runID, "review_run_") {
			core.WriteError(w, validationError("Use a valid review run identity."))
			return
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(runID, "review_run_")); err != nil {
			core.WriteError(w, validationError("Use a valid review run identity."))
			return
		}
		response := RunDetailResponse{ProjectID: scope.ProjectID, Findings: []Finding{}}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, query.Get("snapshot"))
			if err != nil {
				return err
			}
			if !found {
				return notFound("The review snapshot is unavailable.")
			}
			setMetaDetail(&response, meta)
			var pullNum, attributable, complete, finalized int
			var limits string
			err = tx.QueryRowContext(ctx, `SELECT pull_number,id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json FROM review_runs WHERE project_id=? AND snapshot_id=? AND id=?`, scope.ProjectID, meta.SnapshotID, runID).Scan(
				&pullNum, &response.Run.ID, &response.Run.Source, &response.Run.SourceRunID, &response.Run.Channel, &response.Run.HeadSHA, &response.Run.Reviewer, &response.Run.Scope, &response.Run.Outcome, &attributable, &complete, &response.Run.StartedAt, &response.Run.FinishedAt, &response.Run.Found, &response.Run.Accepted, &response.Run.Routed, &finalized, &response.Run.Fixed, &limits)
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("The review run is unavailable in this snapshot.")
			}
			if err != nil {
				return err
			}
			response.PullNumber = int64(pullNum)
			response.Run.PullNumber = int64(pullNum)
			response.Run.Attributable = attributable != 0
			response.Run.Complete = complete != 0
			response.Run.Finalized = finalized != 0
			if err := strictStoredJSON(limits, &response.Run.CoverageLimits); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT source_finding_id,title,detail,severity,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at FROM review_findings WHERE project_id=? AND snapshot_id=? AND pull_number=? AND run_id=? ORDER BY position LIMIT 101`, scope.ProjectID, meta.SnapshotID, pullNum, runID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var finding Finding
				if err := rows.Scan(&finding.SourceFindingID, &finding.Title, &finding.Detail, &finding.Severity, &finding.Disposition, &finding.DecisionReason, &finding.Actor, &finding.DecidedAt, &finding.FixSHA, &finding.ProofNote, &finding.ResolvedAt); err != nil {
					return err
				}
				response.Findings = append(response.Findings, finding)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if len(response.Findings) > 100 {
				return core.Fail("storage_unavailable", "The stored review run exceeds its bounded detail size.", 500)
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		body, err := json.Marshal(response)
		if err != nil || len(body) > 4<<20 {
			core.WriteError(w, core.Fail("response_too_large", "The review run detail exceeds its bounded response size.", 500))
			return
		}
		core.WriteJSON(w, http.StatusOK, response)
	}
}

func selectSnapshot(ctx context.Context, tx *sql.Tx, projectID, requested string) (snapshotMeta, bool, error) {
	if requested != "" {
		var meta snapshotMeta
		err := tx.QueryRowContext(ctx, `SELECT project_id,revision,snapshot_id,source,observed_at,recorded_at FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, projectID, requested).Scan(&meta.ProjectID, &meta.Revision, &meta.SnapshotID, &meta.Source, &meta.ObservedAt, &meta.RecordedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return snapshotMeta{}, false, nil
		}
		return meta, err == nil, err
	}
	var meta snapshotMeta
	err := tx.QueryRowContext(ctx, `SELECT s.project_id,s.revision,s.snapshot_id,s.source,s.observed_at,s.recorded_at FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE w.project_id=?`, projectID).Scan(&meta.ProjectID, &meta.Revision, &meta.SnapshotID, &meta.Source, &meta.ObservedAt, &meta.RecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshotMeta{}, false, nil
	}
	return meta, err == nil, err
}

func pullExists(ctx context.Context, tx *sql.Tx, projectID, snapshotID string, number int64) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?", projectID, snapshotID, number).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, notFound("The pull request is unavailable in this snapshot.")
	}
	return true, err
}
func parsePullNumber(value string) (int64, error) {
	number, err := strconv.ParseInt(value, 10, 32)
	if err != nil || number < 1 || strconv.FormatInt(number, 10) != value {
		return 0, validationError("Use a positive 32-bit pull request number.")
	}
	return number, nil
}
func notFound(message string) error { return core.Fail("not_found", message, http.StatusNotFound) }
func strictStoredJSON(data string, dst any) error {
	if json.Unmarshal([]byte(data), dst) != nil {
		return core.Fail("storage_unavailable", "Stored review projections are invalid.", 500)
	}
	return nil
}
func encodePosition(snapshot string, position int) string {
	return snapshot + "|" + strconv.Itoa(position)
}
func decodePosition(value string) (string, int, error) {
	if value == "" {
		return "", -1, nil
	}
	snapshot, position, ok := strings.Cut(value, "|")
	parsed, err := strconv.Atoi(position)
	if !ok || snapshot == "" || len(snapshot) > 128 || err != nil || parsed < 0 {
		return "", -1, validationError("Use a valid cursor from this review page.")
	}
	return snapshot, parsed, nil
}
func setMetaQueue(response *QueueResponse, meta snapshotMeta) {
	response.ProjectID = meta.ProjectID
	response.Revision = meta.Revision
	response.SnapshotID = meta.SnapshotID
	response.Source = meta.Source
	response.ObservedAt = meta.ObservedAt
	response.RecordedAt = meta.RecordedAt
}
func setMetaPull(response *PullResponse, meta snapshotMeta) error {
	response.ProjectID = meta.ProjectID
	response.Revision = meta.Revision
	response.SnapshotID = meta.SnapshotID
	response.Source = meta.Source
	response.ObservedAt = meta.ObservedAt
	response.RecordedAt = meta.RecordedAt
	return nil
}
func setMetaRuns(response *RunsResponse, meta snapshotMeta) {
	response.ProjectID = meta.ProjectID
	response.Revision = meta.Revision
	response.SnapshotID = meta.SnapshotID
	response.Source = meta.Source
	response.ObservedAt = meta.ObservedAt
	response.RecordedAt = meta.RecordedAt
}
func setMetaDetail(response *RunDetailResponse, meta snapshotMeta) {
	response.ProjectID = meta.ProjectID
	response.Revision = meta.Revision
	response.SnapshotID = meta.SnapshotID
	response.Source = meta.Source
	response.ObservedAt = meta.ObservedAt
	response.RecordedAt = meta.RecordedAt
}
