package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Display-only current obligations are distinct from immutable finalized source
// counts. Selected run versions keep pinned snapshots from seeing later decisions.
// Read scalar inventory for only the displayed page, never finding bodies.
// Finding versions store a validated operational receipt or the canonical 'null'.
func attachRunDisplay(ctx context.Context, tx *sql.Tx, project, snapshot string, groups ...[]RunSummary) error {
	if err := attachDisplayStarts(ctx, tx, project, groups...); err != nil {
		return err
	}
	runs := map[string][]*RunSummary{}
	ids := []string{}
	for _, group := range groups {
		for i := range group {
			run := &group[i]
			if _, exists := runs[run.ID]; !exists {
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
	rows, err := tx.QueryContext(ctx, `SELECT r.id,count(f.source_finding_id),
 coalesce(sum(CASE WHEN f.disposition='accepted' THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN f.disposition='accepted' AND f.fix_sha='' AND f.operational_proof_json='null' THEN 1 ELSE 0 END),0)
 FROM json_each(?) selected JOIN review_runs r ON r.id=selected.value
 LEFT JOIN review_finding_versions f ON f.project_id=r.project_id AND f.run_id=r.id
 AND f.pull_number=r.pull_number AND f.run_version_revision=r.version_revision
 WHERE r.project_id=? AND r.snapshot_id=? GROUP BY r.id`, string(selected), project, snapshot)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var total, accepted, pending int64
		if err := rows.Scan(&id, &total, &accepted, &pending); err != nil {
			return err
		}
		for _, run := range runs[id] {
			if run.Complete && total == run.Found || isPartialCLIResult(*run) && total > 0 {
				// Partial CLI projections describe retained findings only, not a completed round.
				currentAccepted, currentPending := accepted, pending
				run.CurrentAccepted, run.CurrentPendingFixes = &currentAccepted, &currentPending
			}
		}
	}
	return rows.Err()
}
