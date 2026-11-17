package reviews

import (
	"context"
	"database/sql"
	"errors"
	"regexp"

	"github.com/benhook1013/FireController/internal/core"
)

// Archived producer labels were also available to history imports. Only the
// observer's capture event proves the original run came through verification.
const externalHostedCaptureProof = `EXISTS(SELECT 1 FROM events e
 WHERE e.project_id=s.project_id AND e.resource_id=s.snapshot_id AND e.revision=s.revision
 AND e.event_type='review.observed'
 AND json_extract(e.payload_json,'$.source')='github-hosted-observer'
 AND json_extract(e.payload_json,'$.pull_number')=v.pull_number
 AND json_type(e.payload_json,'$.native_coverage')='false')`

var externalHostedRunID = regexp.MustCompile("^review_run_[0-9a-f]{32}$")

// ExternalHostedRunID returns the stable history identity for a provider run.
func ExternalHostedRunID(project, sourceID string) string {
	if !externalHostedIdentity.MatchString(sourceID) {
		return ""
	}
	return runOpaqueID(project, externalHostedSource, sourceID)
}

// PendingExternalHosted reads bounded IDs from the current membership of one
// PR. Superseded observer copies of native captures do not become obligations.
func PendingExternalHosted(ctx context.Context, q HistoryQuery, project string, repository, pull int64, limit int) ([]string, error) {
	condition := ""
	args := []any{project, externalHostedSource, repository}
	if pull != 0 {
		condition = " AND c.pull_number=?"
		args = append(args, pull)
	}
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, `SELECT v.id
 FROM review_current_runs c
 JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
 JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision
 JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=v.version_revision
 WHERE c.project_id=? AND v.source=? AND v.channel='hosted'
 AND v.complete=1 AND v.attributable=1 AND v.outcome='completed' AND v.finalized=0 AND s.repository_id=? AND `+
		externalHostedCaptureProof+condition+` ORDER BY c.run_order LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func ExternalHostedSettlementRun(ctx context.Context, tx *sql.Tx, project string, repository, pull int64, id string) (RunDetailResponse, error) {
	refused := notFound("The completed external Hosted run is unavailable for settlement on this PR.")
	if !externalHostedRunID.MatchString(id) {
		return RunDetailResponse{}, refused
	}
	var meta snapshotMeta
	meta.ProjectID = project
	err := tx.QueryRowContext(ctx, `SELECT current.snapshot_id FROM review_workspace w
 JOIN review_snapshots current ON current.project_id=w.project_id AND current.snapshot_id=w.snapshot_id
 JOIN review_current_runs c ON c.project_id=w.project_id AND c.id=?
 JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
 JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision
 JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=(
   SELECT min(initial.version_revision) FROM review_run_versions initial WHERE initial.project_id=v.project_id AND initial.id=v.id)
 WHERE w.project_id=? AND current.repository_id=? AND s.repository_id=? AND `+externalHostedCaptureProof, id, project, repository, repository).Scan(&meta.SnapshotID)
	if errors.Is(err, sql.ErrNoRows) {
		return RunDetailResponse{}, refused
	}
	if err != nil {
		return RunDetailResponse{}, err
	}
	detail, err := readRunDetail(ctx, tx, meta, id)
	if err != nil {
		return RunDetailResponse{}, err
	}
	run := detail.Run
	if run.Source != externalHostedSource || run.PullNumber != pull || run.Channel != "hosted" ||
		!run.Complete || !run.Attributable || run.Outcome != "completed" ||
		run.Found != int64(len(detail.Findings)) || ExternalHostedRunID(project, run.SourceRunID) != id {
		return RunDetailResponse{}, refused
	}
	if run.Finalized {
		return RunDetailResponse{}, nativeAdjudicationConflict()
	}
	return detail, nil
}

func FinalizeExternalHostedRun(ctx context.Context, tx *sql.Tx, scope core.Scope, input NativeAdjudication) error {
	_, changed, err := updateSourceAdjudication(ctx, tx, scope, externalHostedSource, input)
	if err == nil && !changed {
		return nativeAdjudicationConflict()
	}
	return err
}

// ExternalHostedFinalizedRunAccepted supplies maintenance proof for round
// replay. A run's final version remains immutable even after membership ends.
func ExternalHostedFinalizedRunAccepted(ctx context.Context, q HistoryQuery, project string, pull int64, id string) (int64, error) {
	var sourceID string
	var accepted, found, revision int64
	err := q.QueryRowContext(ctx, `SELECT v.source_run_id,v.accepted,v.found,v.version_revision FROM review_run_versions v
 JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=(
   SELECT min(initial.version_revision) FROM review_run_versions initial WHERE initial.project_id=v.project_id AND initial.id=v.id)
 WHERE v.project_id=? AND v.id=? AND v.pull_number=? AND v.source=? AND v.channel='hosted'
 AND v.complete=1 AND v.attributable=1 AND v.outcome='completed' AND v.finalized=1 AND v.accepted=v.fixed AND v.routed=0
 AND `+externalHostedCaptureProof+` ORDER BY v.version_revision DESC LIMIT 1`, project, id, pull, externalHostedSource).Scan(&sourceID, &accepted, &found, &revision)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (ExternalHostedRunID(project, sourceID) != id || accepted < 0 || found < accepted || found > 100) {
		return 0, historyInconsistent()
	}
	if err != nil {
		return 0, err
	}
	var findings, fixed, rejected int64
	err = q.QueryRowContext(ctx, `SELECT count(*),
 coalesce(sum(disposition='accepted' AND length(fix_sha)=40 AND fix_sha NOT GLOB '*[^0-9a-f]*' AND trim(proof_note)!='' AND resolved_at!=''),0),
 coalesce(sum(disposition='rejected' AND fix_sha='' AND proof_note='' AND resolved_at=''),0)
 FROM review_finding_versions WHERE project_id=? AND run_id=? AND run_version_revision=?`, project, id, revision).Scan(&findings, &fixed, &rejected)
	if err != nil {
		return 0, err
	}
	if findings != found || fixed != accepted || rejected != found-accepted {
		return 0, historyInconsistent()
	}
	return accepted, nil
}
