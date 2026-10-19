package reviews

import (
	"context"
	"database/sql"
	"strings"
)

func (w *historySnapshotWrite) pull(ctx context.Context, tx *sql.Tx, number int64, position int, body []any) error {
	version, err := w.version(ctx, tx, historyPullRecord, []any{number}, body, false)
	if err != nil {
		return err
	}
	return w.membership(ctx, tx, historyPullRecord, []any{number}, version, []any{position})
}
func (w *historySnapshotWrite) retired(ctx context.Context, tx *sql.Tx, number int64, position int, reason string) error {
	version, err := w.version(ctx, tx, historyRetiredRecord, []any{number}, []any{reason}, false)
	if err != nil {
		return err
	}
	return w.membership(ctx, tx, historyRetiredRecord, []any{number}, version, []any{position})
}

// A full observation is an explicit complete bounded replacement of current
// membership. Immutable bodies remain retained across omissions and returns.
func (w *historySnapshotWrite) observation(ctx context.Context, tx *sql.Tx, req ObservationRequest) error {
	selected := map[string]map[string]bool{}
	totals := make(map[int64]summaryTotals, len(req.Queue))
	for _, spec := range []historyRecordSpec{historyPullRecord, historyRunRecord, historyAttemptRecord, historyRetiredRecord} {
		selected[spec.memberships] = map[string]bool{}
	}
	keep := func(spec historyRecordSpec, keys ...any) { selected[spec.memberships][mustJSON(keys)] = true }
	for position, pull := range req.Queue {
		total := summaryTotals{runs: int64(len(pull.Runs)), attempts: int64(len(pull.Attempts))}
		for _, run := range pull.Runs {
			if !run.Complete {
				total.incomplete++
			}
		}
		totals[pull.Number] = total
		recent := make([]RunSummary, 0, 3)
		for i := len(pull.Runs) - 1; i >= 0 && len(recent) < 3; i-- {
			recent = append(recent, summarizeRun(w.project, req.Source, pull.Number, pull.Runs[i]))
		}
		body := []any{pull.Title, pull.State, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, pull.CIState, pull.Mergeability, pull.Complete, len(pull.Runs), len(pull.Attempts), mustJSON(recent), mustJSON(latestAttempts(pull.Attempts)), mustJSON(pullMetadata(pull))}
		if err := w.pull(ctx, tx, pull.Number, position, body); err != nil {
			return err
		}
		keep(historyPullRecord, pull.Number)
		for order, run := range pull.Runs {
			if err := w.run(ctx, tx, req.Source, pull.Number, order, run); err != nil {
				return err
			}
			keep(historyRunRecord, runOpaqueID(w.project, req.Source, run.SourceRunID))
		}
		for order, attempt := range pull.Attempts {
			if err := w.attempt(ctx, tx, req.Source, pull.Number, order, attempt); err != nil {
				return err
			}
			keep(historyAttemptRecord, req.Source, attempt.SourceAttemptID)
		}
	}
	for position, retired := range req.Retired {
		if err := w.retired(ctx, tx, retired.Number, position, retired.Reason); err != nil {
			return err
		}
		keep(historyRetiredRecord, retired.Number)
	}
	for _, spec := range []historyRecordSpec{historyPullRecord, historyRunRecord, historyAttemptRecord, historyRetiredRecord} {
		if err := w.closeOmitted(ctx, tx, spec, selected[spec.memberships]); err != nil {
			return err
		}
	}
	w.replacementTotals = totals
	return nil
}

func (w *historySnapshotWrite) closeOmitted(ctx context.Context, tx *sql.Tx, spec historyRecordSpec, selected map[string]bool) error {
	// Stable source identity is selected globally, before any PR/position filter.
	rows, err := tx.QueryContext(ctx, "SELECT "+strings.Join(spec.keys, ",")+" FROM "+spec.memberships+" WHERE project_id=? AND end_revision IS NULL", w.project)
	if err != nil {
		return err
	}
	var omitted [][]any
	for rows.Next() {
		keys, targets := make([]any, len(spec.keys)), make([]any, len(spec.keys))
		for i := range keys {
			targets[i] = &keys[i]
		}
		if err := rows.Scan(targets...); err != nil {
			rows.Close()
			return err
		}
		if !selected[mustJSON(keys)] {
			omitted = append(omitted, keys)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Finish enumerating before changing the same membership table.
	for _, keys := range omitted {
		if err := w.remove(ctx, tx, spec, keys); err != nil {
			return err
		}
	}
	return nil
}
