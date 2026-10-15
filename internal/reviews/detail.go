package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
)

// Include the final newline written by core.WriteJSON's JSON encoder.
const maxRunDetailBytes = 4 << 20

// readRunDetail is shared by public reads and pre-commit native capacity checks.
func readRunDetail(ctx context.Context, tx *sql.Tx, meta snapshotMeta, runID string) (RunDetailResponse, error) {
	response := RunDetailResponse{ProjectID: meta.ProjectID, Findings: []Finding{}}
	setMetaDetail(&response, meta)
	var pullNum, attributable, complete, finalized int
	var limits string
	err := tx.QueryRowContext(ctx, `SELECT pull_number,id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json FROM review_runs WHERE project_id=? AND snapshot_id=? AND id=?`, meta.ProjectID, meta.SnapshotID, runID).Scan(
		&pullNum, &response.Run.ID, &response.Run.Source, &response.Run.SourceRunID, &response.Run.Channel, &response.Run.HeadSHA, &response.Run.Reviewer, &response.Run.Scope, &response.Run.Outcome, &attributable, &complete, &response.Run.StartedAt, &response.Run.FinishedAt, &response.Run.Found, &response.Run.Accepted, &response.Run.Routed, &finalized, &response.Run.Fixed, &limits)
	if errors.Is(err, sql.ErrNoRows) {
		return response, notFound("The review run is unavailable in this snapshot.")
	}
	if err != nil {
		return response, err
	}
	response.PullNumber = int64(pullNum)
	response.Run.PullNumber = int64(pullNum)
	response.Run.Attributable = attributable != 0
	response.Run.Complete = complete != 0
	response.Run.Finalized = finalized != 0
	if err := strictStoredJSON(limits, &response.Run.CoverageLimits); err != nil {
		return response, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_finding_id,title,detail,severity,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at,route_json FROM review_findings WHERE project_id=? AND snapshot_id=? AND pull_number=? AND run_id=? ORDER BY position LIMIT 101`, meta.ProjectID, meta.SnapshotID, pullNum, runID)
	if err != nil {
		return response, err
	}
	defer rows.Close()
	for rows.Next() {
		var finding Finding
		var routeJSON string
		if err := rows.Scan(&finding.SourceFindingID, &finding.Title, &finding.Detail, &finding.Severity, &finding.Disposition, &finding.DecisionReason, &finding.Actor, &finding.DecidedAt, &finding.FixSHA, &finding.ProofNote, &finding.ResolvedAt, &routeJSON); err != nil {
			return response, err
		}
		finding.Route, err = decodeFindingRoute(routeJSON, int64(pullNum))
		if err != nil || finding.Route != nil && finding.Disposition != "routed" {
			if err == nil {
				err = core.Fail("storage_unavailable", "Stored finding route is unavailable.", 500)
			}
			return response, err
		}
		response.Findings = append(response.Findings, finding)
	}
	if err := rows.Err(); err != nil {
		return response, err
	}
	if len(response.Findings) > 100 {
		return response, core.Fail("storage_unavailable", "The stored review run exceeds its bounded detail size.", 500)
	}
	return response, nil
}

func encodedRunDetailSize(response RunDetailResponse) (int, error) {
	body, err := json.Marshal(response)
	return len(body) + 1, err
}

// Only the touched run is loaded; no unrelated run history is scanned.
func checkNativeRunDetail(ctx context.Context, tx *sql.Tx, project, snapshot, runID string) error {
	meta, found, err := selectSnapshot(ctx, tx, project, snapshot)
	if err != nil {
		return err
	}
	if !found {
		return notFound("The review snapshot is unavailable.")
	}
	response, err := readRunDetail(ctx, tx, meta, runID)
	if err != nil {
		return err
	}
	size, err := encodedRunDetailSize(response)
	if err != nil {
		return err
	}
	if size > maxRunDetailBytes {
		return core.Fail("workspace_capacity", "The retained review run exceeds its bounded detail size; no history was dropped.", 409)
	}
	return nil
}
