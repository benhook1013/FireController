package reviews

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// All identifiers come from repository-owned specifications. Record values
// remain parameters, including original producer and external source IDs.
type historyRecordSpec struct {
	versions, memberships, current string
	keys, body, placement          []string
}

type historyCurrentChange struct {
	spec   historyRecordSpec
	keys   []any
	values []any // membership start revision followed by placement; nil removes it
}

// Current references stay unchanged until all version writes and preflight
// reads finish. Historical membership can already describe the new snapshot
// without changing how the previous current snapshot resolves its records.
type historySnapshotWrite struct {
	project  string
	revision int64
	pending  map[string]historyCurrentChange
}

func newHistorySnapshotWrite(project string, revision int64) *historySnapshotWrite {
	return &historySnapshotWrite{project: project, revision: revision, pending: map[string]historyCurrentChange{}}
}

func historyInconsistent() error {
	return core.Fail("storage_unavailable", "Retained review history is inconsistent.", 500)
}

func historyParameters(values []any) ([]any, error) {
	out := make([]any, len(values))
	for i, value := range values {
		switch v := value.(type) {
		case nil, string, int64:
			out[i] = v
		case int:
			out[i] = int64(v)
		case bool:
			out[i] = int64(0)
			if v {
				out[i] = int64(1)
			}
		case *int64:
			if v != nil {
				out[i] = *v
			}
		default:
			return nil, historyInconsistent()
		}
	}
	return out, nil
}

func historyPredicate(columns []string) string {
	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = column + "=?"
	}
	return strings.Join(parts, " AND ")
}

func historyInsert(ctx context.Context, tx *sql.Tx, table string, columns []string, values []any) error {
	if len(columns) != len(values) || len(columns) == 0 {
		return historyInconsistent()
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+table+"("+strings.Join(columns, ",")+") VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")+")", values...)
	return err
}

// A run's caller sets changedDetail when its bounded findings changed even if
// the scalar summary did not. That creates a new exact run/detail version.
func (w *historySnapshotWrite) version(ctx context.Context, tx *sql.Tx, spec historyRecordSpec, keys, body []any, changedDetail bool) (int64, error) {
	if w.project == "" || w.revision < 1 || len(keys) != len(spec.keys) || len(body) != len(spec.body) {
		return 0, historyInconsistent()
	}
	keys, err := historyParameters(keys)
	if err != nil {
		return 0, err
	}
	body, err = historyParameters(body)
	if err != nil {
		return 0, err
	}
	args := append([]any{w.project}, keys...)
	var previous int64
	stored := make([]any, len(body))
	targets := []any{&previous}
	for i := range stored {
		targets = append(targets, &stored[i])
	}
	columns := append([]string{"version_revision"}, spec.body...)
	err = tx.QueryRowContext(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+spec.versions+" WHERE project_id=? AND "+historyPredicate(spec.keys)+" ORDER BY version_revision DESC LIMIT 1", args...).Scan(targets...)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err == nil {
		if previous < 1 || previous > w.revision {
			return 0, historyInconsistent()
		}
		if !changedDetail && reflect.DeepEqual(stored, body) {
			return previous, nil
		}
		if previous == w.revision {
			// A staged immutable version may be retried exactly, never replaced.
			return 0, historyInconsistent()
		}
	}
	columns = append([]string{"project_id"}, spec.keys...)
	columns = append(columns, "version_revision")
	columns = append(columns, spec.body...)
	values := append(args, w.revision)
	values = append(values, body...)
	if err := historyInsert(ctx, tx, spec.versions, columns, values); err != nil {
		return 0, err
	}
	return w.revision, nil
}

func (w *historySnapshotWrite) membership(ctx context.Context, tx *sql.Tx, spec historyRecordSpec, keys []any, version int64, placement []any) error {
	if len(keys) != len(spec.keys) || len(placement) != len(spec.placement) || version < 1 || version > w.revision {
		return historyInconsistent()
	}
	keys, err := historyParameters(keys)
	if err != nil {
		return err
	}
	placement, err = historyParameters(placement)
	if err != nil {
		return err
	}
	args := append([]any{w.project}, keys...)
	var previous, previousVersion int64
	stored := make([]any, len(placement))
	targets := []any{&previous, &previousVersion}
	for i := range stored {
		targets = append(targets, &stored[i])
	}
	columns := append([]string{"start_revision", "version_revision"}, spec.placement...)
	where := "project_id=? AND " + historyPredicate(spec.keys) + " AND end_revision IS NULL"
	err = tx.QueryRowContext(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+spec.memberships+" WHERE "+where, args...).Scan(targets...)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if previous < 1 || previous > w.revision {
			return historyInconsistent()
		}
		if previousVersion == version && reflect.DeepEqual(stored, placement) {
			return nil
		}
		if previous == w.revision {
			return historyInconsistent()
		}
		result, err := tx.ExecContext(ctx, "UPDATE "+spec.memberships+" SET end_revision=? WHERE "+where+" AND start_revision=?", append(append([]any{w.revision}, args...), previous)...)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return historyInconsistent()
		}
	}
	columns = append([]string{"project_id"}, spec.keys...)
	columns = append(columns, "start_revision", "version_revision")
	columns = append(columns, spec.placement...)
	values := append(append([]any{}, args...), w.revision, version)
	values = append(values, placement...)
	if err := historyInsert(ctx, tx, spec.memberships, columns, values); err != nil {
		return err
	}
	w.pending[spec.current+":"+mustJSON(keys)] = historyCurrentChange{spec: spec, keys: keys, values: append([]any{w.revision}, placement...)}
	return nil
}

func (w *historySnapshotWrite) remove(ctx context.Context, tx *sql.Tx, spec historyRecordSpec, keys []any) error {
	if len(keys) != len(spec.keys) || w.revision < 1 {
		return historyInconsistent()
	}
	keys, err := historyParameters(keys)
	if err != nil {
		return err
	}
	args := append([]any{w.project}, keys...)
	where := "project_id=? AND " + historyPredicate(spec.keys) + " AND end_revision IS NULL"
	var start int64
	err = tx.QueryRowContext(ctx, "SELECT start_revision FROM "+spec.memberships+" WHERE "+where, args...).Scan(&start)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if start < 1 || start >= w.revision {
		return historyInconsistent()
	}
	result, err := tx.ExecContext(ctx, "UPDATE "+spec.memberships+" SET end_revision=? WHERE "+where+" AND start_revision=?", append(append([]any{w.revision}, args...), start)...)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return historyInconsistent()
	}
	w.pending[spec.current+":"+mustJSON(keys)] = historyCurrentChange{spec: spec, keys: keys}
	return nil
}

// Installing references and advancing the workspace are one storage boundary.
// The caller performs no external work inside its journal transaction.
func (w *historySnapshotWrite) applyCurrent(ctx context.Context, tx *sql.Tx, snapshot string, previousRevision int64) error {
	if previousRevision < 0 || w.revision < 1 || w.revision-1 != previousRevision {
		return historyInconsistent()
	}
	var recordedRevision int64
	if err := tx.QueryRowContext(ctx, "SELECT revision FROM review_snapshots WHERE project_id=? AND snapshot_id=?", w.project, snapshot).Scan(&recordedRevision); err != nil {
		return err
	}
	if recordedRevision != w.revision {
		return historyInconsistent()
	}
	totals, err := w.prepareCurrentTotals(ctx, tx)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(w.pending))
	for key := range w.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Delete all touched references before inserting replacements so valid
	// swaps of queue positions do not collide with the old unique ordering.
	for _, key := range keys {
		change := w.pending[key]
		args := append([]any{w.project}, change.keys...)
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+change.spec.current+" WHERE project_id=? AND "+historyPredicate(change.spec.keys), args...); err != nil {
			return err
		}
	}
	for _, key := range keys {
		change := w.pending[key]
		if change.values == nil {
			continue
		}
		columns := append([]string{"project_id"}, change.spec.keys...)
		columns = append(columns, "start_revision")
		columns = append(columns, change.spec.placement...)
		values := append([]any{w.project}, change.keys...)
		values = append(values, change.values...)
		if err := historyInsert(ctx, tx, change.spec.current, columns, values); err != nil {
			return err
		}
	}
	var result sql.Result
	if previousRevision == 0 {
		result, err = tx.ExecContext(ctx, "INSERT INTO review_workspace(project_id,revision,snapshot_id) VALUES(?,?,?)", w.project, w.revision, snapshot)
	} else {
		result, err = tx.ExecContext(ctx, "UPDATE review_workspace SET revision=?,snapshot_id=? WHERE project_id=? AND revision=?", w.revision, snapshot, w.project, previousRevision)
	}
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return historyInconsistent()
	}
	return w.installCurrentSummaries(ctx, tx, totals)
}
