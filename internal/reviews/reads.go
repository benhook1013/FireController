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

func queue(store *core.Store, health ...ObservationHealthReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "validation_failed", "limit", "cursor", "view", "include_observation", "include_merge_status")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if selected, ok := query["include_observation"]; ok && (len(selected) != 1 || selected[0] != "true") {
			core.WriteError(w, validationError("Use include_observation=true for runtime observer health."))
			return
		}
		if selected, ok := query["include_merge_status"]; ok && (len(selected) != 1 || selected[0] != "true") {
			core.WriteError(w, validationError("Use include_merge_status=true for the browser merge projection."))
			return
		}
		selectedHealth := health
		if query.Get("include_observation") != "true" {
			selectedHealth = nil
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
		if _, selected := query["view"]; selected {
			view := query.Get("view")
			if view != "recent" && view != "merged" {
				core.WriteError(w, validationError("Use recent or merged for the queue view."))
				return
			}
			response, err := readQueueWindow(r.Context(), store, scope, view, query.Get("cursor"), limit)
			if err == nil {
				err = attachObservationHealth(r.Context(), scope.ProjectID, &response, selectedHealth)
			}
			if err != nil {
				core.WriteError(w, err)
				return
			}
			if query.Get("include_merge_status") == "true" {
				includeMergeStatus(response.Items)
			}
			core.WriteJSON(w, http.StatusOK, response)
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
			rows, err := tx.QueryContext(ctx, `SELECT p.number,p.title,p.state,p.head_sha,p.base_ref,p.base_sha,p.ci_state,p.mergeability,p.complete,p.run_count,p.attempt_count,p.recent_runs_json,p.latest_attempts_json,p.metadata_json,p.position
				FROM review_pulls p WHERE p.project_id=? AND p.snapshot_id=? AND p.position>? AND NOT EXISTS (
					SELECT 1 FROM review_retired_pulls retired WHERE retired.project_id=p.project_id AND retired.snapshot_id=p.snapshot_id AND retired.number=p.number
				) ORDER BY p.position LIMIT ?`,
				scope.ProjectID, meta.SnapshotID, position, limit+1)
			if err != nil {
				return err
			}
			defer rows.Close()
			lastPosition := -1
			for rows.Next() {
				var item PullSummary
				var complete int
				var recentJSON, latestJSON, metadataJSON string
				var rowPosition int
				if err := rows.Scan(&item.Number, &item.Title, &item.State, &item.HeadSHA, &item.BaseRef, &item.BaseSHA, &item.CIState, &item.Mergeability, &complete, &item.RunCount, &item.AttemptCount, &recentJSON, &latestJSON, &metadataJSON, &rowPosition); err != nil {
					return err
				}
				if len(response.Items) == limit {
					response.HasMore = true
					break
				}
				m, err := ReadPullMetadata(metadataJSON, item.State)
				if err != nil {
					return err
				}
				item.Diff, item.IsDraft, item.MergedAt, item.mergeStateStatus = m.Diff, m.IsDraft, m.MergedAt, m.MergeStateStatus
				item.Complete = complete != 0
				if err := fillChannelRunCounts(ctx, tx, scope.ProjectID, meta, &item); err != nil {
					return err
				}
				if err := strictStoredJSON(recentJSON, &item.RecentRuns); err != nil {
					return err
				}
				if err := fillRecentRunSources(ctx, tx, scope.ProjectID, meta.SnapshotID, item.Number, item.RecentRuns); err != nil {
					return err
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
		if err := attachObservationHealth(r.Context(), scope.ProjectID, &response, selectedHealth); err != nil {
			core.WriteError(w, err)
			return
		}
		if query.Get("include_merge_status") == "true" {
			includeMergeStatus(response.Items)
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
		query, err := core.QueryKeys(r, "validation_failed", "snapshot", "include_merge_status")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if selected, ok := query["include_merge_status"]; ok && (len(selected) != 1 || selected[0] != "true") {
			core.WriteError(w, validationError("Use include_merge_status=true for the browser merge projection."))
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
		response := PullResponse{ProjectID: scope.ProjectID}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, query.Get("snapshot"))
			if err != nil {
				return err
			}
			if !found {
				return notFound("The review snapshot is unavailable.")
			}
			setMetaPull(&response, meta)
			var complete int
			var recentJSON, latestJSON, metadataJSON string
			err = tx.QueryRowContext(ctx, `SELECT number,title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete,run_count,attempt_count,recent_runs_json,latest_attempts_json,metadata_json FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?`, scope.ProjectID, meta.SnapshotID, number).Scan(
				&response.Pull.Number, &response.Pull.Title, &response.Pull.State, &response.Pull.HeadSHA, &response.Pull.BaseRef, &response.Pull.BaseSHA, &response.Pull.CIState, &response.Pull.Mergeability, &complete, &response.Pull.RunCount, &response.Pull.AttemptCount, &recentJSON, &latestJSON, &metadataJSON)
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("The pull request is unavailable in this snapshot.")
			}
			if err != nil {
				return err
			}
			m, err := ReadPullMetadata(metadataJSON, response.Pull.State)
			if err != nil {
				return err
			}
			response.Pull.Diff, response.Pull.IsDraft, response.Pull.MergedAt, response.Pull.mergeStateStatus = m.Diff, m.IsDraft, m.MergedAt, m.MergeStateStatus
			response.Pull.Complete = complete != 0
			if err := fillChannelRunCounts(ctx, tx, scope.ProjectID, meta, &response.Pull); err != nil {
				return err
			}
			if err := strictStoredJSON(recentJSON, &response.Pull.RecentRuns); err != nil {
				return err
			}
			if err := fillRecentRunSources(ctx, tx, scope.ProjectID, meta.SnapshotID, number, response.Pull.RecentRuns); err != nil {
				return err
			}
			if err := strictStoredJSON(latestJSON, &response.Pull.LatestAttempts); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if query.Get("include_merge_status") == "true" {
			response.Pull.MergeStatus = mergeStatus(response.Pull)
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
		query, err := core.QueryKeys(r, "validation_failed", "snapshot", "limit", "cursor", "channel", "view")
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
		channel := query.Get("channel")
		if _, selected := query["channel"]; selected && !oneOf(channel, "hosted", "cli", "subagent", "manual") {
			core.WriteError(w, validationError("Select hosted, cli, subagent or manual for the runs channel."))
			return
		}
		if view, selected := query["view"]; selected {
			if len(view) != 1 || view[0] != "timeline" {
				core.WriteError(w, validationError("Select timeline for the runs view."))
				return
			}
			serveTimelineRuns(w, r, store, scope, number, channel, limit, query.Get("snapshot"), query.Get("cursor"))
			return
		}
		filter := fmt.Sprintf("reviews.runs:%d:v1", number)
		if channel != "" {
			filter += ":channel=" + channel
		}
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
			if channel != "" {
				querySQL += ` AND channel=?`
				args = append(args, channel)
			}
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
		query, err := core.QueryKeys(r, "validation_failed", "snapshot", "view")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if len(r.URL.RawQuery) > 4096 {
			core.WriteError(w, validationError("The query exceeds its bounded size."))
			return
		}
		if view, selected := query["view"]; selected && (len(view) != 1 || view[0] != "display") {
			core.WriteError(w, validationError("Select display for the run detail view."))
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
		response := RunDetailResponse{ProjectID: scope.ProjectID, Findings: []FindingView{}}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, query.Get("snapshot"))
			if err != nil {
				return err
			}
			if !found {
				return notFound("The review snapshot is unavailable.")
			}
			response, err = readRunDetail(ctx, tx, meta, runID)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if query.Get("view") == "display" {
			addFindingDisplaySeverity(&response)
		}
		body, err := json.Marshal(response)
		if err != nil || len(body)+1 > maxRunDetailBytes {
			core.WriteError(w, core.Fail("response_too_large", "The review run detail exceeds its bounded response size.", 500))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if w.Header().Get("X-Request-ID") == "" {
			w.Header().Set("X-Request-ID", core.NewID("request"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(append(body, '\n'))
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
	if summaries, ok := dst.(*[]AttemptSummary); ok {
		var fields []map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &fields) != nil {
			return historyInconsistent()
		}
		for i, item := range *summaries {
			_, provided := fields[i]["reason"]
			if !ValidAttemptReason(item.State, item.Reason) || provided && item.Reason == "" {
				return historyInconsistent()
			}
		}
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
func setMetaPull(response *PullResponse, meta snapshotMeta) {
	response.ProjectID = meta.ProjectID
	response.Revision = meta.Revision
	response.SnapshotID = meta.SnapshotID
	response.Source = meta.Source
	response.ObservedAt = meta.ObservedAt
	response.RecordedAt = meta.RecordedAt
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

// Historical compact summaries may omit source. A mixed snapshot's publisher
// cannot supply that identity; resolve only the exact retained run in this PR.
func fillRecentRunSources(ctx context.Context, tx *sql.Tx, project, snapshot string, number int64, runs []RunSummary) error {
	for i := range runs {
		if runs[i].Source != "" {
			continue
		}
		if err := tx.QueryRowContext(ctx, `SELECT source FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=? AND id=?`, project, snapshot, number, runs[i].ID).Scan(&runs[i].Source); err != nil {
			return err
		}
	}
	return nil
}
