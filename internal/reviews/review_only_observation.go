package reviews

import (
	"context"
	"database/sql"
	"strings"
)

// Full Delivery replacement cannot discard separately recorded worker reviews.
// Move their catalog placements after the explicit queue and retain their exact
// immutable bodies and child memberships. Queue positions are not review IDs.
func (w *historySnapshotWrite) preserveReviewOnly(ctx context.Context, tx *sql.Tx, req ObservationRequest, selected map[string]map[string]bool, totals map[int64]summaryTotals) error {
	rows, err := tx.QueryContext(ctx, `SELECT c.number FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE c.project_id=? AND v.delivery_selected=0 ORDER BY c.position`, w.project)
	if err != nil {
		return err
	}
	var numbers []int64
	for rows.Next() {
		var n int64
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		if !selected[historyPullRecord.memberships][mustJSON([]any{n})] {
			numbers = append(numbers, n)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i, number := range numbers {
		body, _, err := retainedTransferPullBody(ctx, tx, w.project, number)
		if err != nil {
			return err
		}
		if err = w.pull(ctx, tx, number, len(req.Queue)+i, body); err != nil {
			return err
		}
		selected[historyPullRecord.memberships][mustJSON([]any{number})] = true
		total, err := readSummaryTotals(ctx, tx, w.project, number)
		if err != nil {
			return err
		}
		totals[number] = total
		for _, spec := range []historyRecordSpec{historyRunRecord, historyAttemptRecord} {
			children, err := tx.QueryContext(ctx, "SELECT "+strings.Join(spec.keys, ",")+" FROM "+spec.current+" WHERE project_id=? AND pull_number=?", w.project, number)
			if err != nil {
				return err
			}
			for children.Next() {
				keys, targets := make([]any, len(spec.keys)), make([]any, len(spec.keys))
				for j := range keys {
					targets[j] = &keys[j]
				}
				if err = children.Scan(targets...); err != nil {
					children.Close()
					return err
				}
				selected[spec.memberships][mustJSON(keys)] = true
			}
			err = children.Err()
			children.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
