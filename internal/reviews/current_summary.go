package reviews

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CurrentSummarySchemaSQL defines derived current indexes, never historical
// authority. Cascades remove only indexes whose owning current reference ends.
func CurrentSummarySchemaSQL() []string {
	return []string{
		`CREATE TABLE review_current_totals (
 project_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 run_count INTEGER NOT NULL CHECK(run_count>=0),
 attempt_count INTEGER NOT NULL CHECK(attempt_count>=0),
 incomplete_run_count INTEGER NOT NULL CHECK(incomplete_run_count>=0 AND incomplete_run_count<=run_count),
 PRIMARY KEY(project_id,pull_number),
 FOREIGN KEY(project_id,pull_number) REFERENCES review_current_pulls(project_id,number) ON DELETE CASCADE
);`,
		`CREATE TABLE review_current_attempt_order (
 project_id TEXT NOT NULL,
 source TEXT NOT NULL,
 source_attempt_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 channel TEXT NOT NULL,
 sort_key TEXT NOT NULL,
 position INTEGER NOT NULL CHECK(position>=0),
 PRIMARY KEY(project_id,source,source_attempt_id),
 FOREIGN KEY(project_id,source,source_attempt_id) REFERENCES review_current_attempts(project_id,source,source_attempt_id) ON DELETE CASCADE
);`,
		`CREATE INDEX review_current_attempt_order_latest ON review_current_attempt_order(project_id,pull_number,channel,sort_key DESC,position DESC);`,
	}
}

type summaryTotals struct{ runs, attempts, incomplete int64 }

type currentSummarySQL interface {
	HistoryQuery
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Raw RFC3339 strings do not sort by instant across offsets or fractional
// precision. Preserve nanoseconds rather than SQLite's millisecond conversion.
func attemptSortKey(attempt Attempt) (string, error) {
	for _, value := range []string{attempt.StartedAt, attempt.FinishedAt} {
		if _, err := parseTimestamp(value, false); err != nil {
			return "", historyInconsistent()
		}
	}
	t := attemptOrder(attempt)
	// Bias signed Unix seconds into unsigned order, then append nanoseconds.
	// This also preserves valid RFC3339 boundary dates whose offset takes the
	// UTC year outside the four-digit formatting range.
	return fmt.Sprintf("%016x.%09d", uint64(t.Unix())^(uint64(1)<<63), t.Nanosecond()), nil
}

const currentAttemptRows = `SELECT c.project_id,c.source,c.source_attempt_id,c.pull_number,c.position,v.channel,v.started_at,v.finished_at
 FROM review_current_attempts c
 JOIN review_attempt_memberships m ON m.project_id=c.project_id AND m.source=c.source AND m.source_attempt_id=c.source_attempt_id AND m.start_revision=c.start_revision
 JOIN review_attempt_versions v ON v.project_id=m.project_id AND v.source=m.source AND v.source_attempt_id=m.source_attempt_id AND v.version_revision=m.version_revision`

// The core owner invokes this once inside the exclusive schema transaction.
// No provider, filesystem wait or second connection participates in backfill.
func backfillCurrentSummaries(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `INSERT INTO review_current_totals(project_id,pull_number,run_count,attempt_count,incomplete_run_count)
 SELECT p.project_id,p.number,
 (SELECT count(*) FROM review_current_runs r WHERE r.project_id=p.project_id AND r.pull_number=p.number),
 (SELECT count(*) FROM review_current_attempts a WHERE a.project_id=p.project_id AND a.pull_number=p.number),
 (SELECT count(*) FROM review_current_runs r JOIN review_run_memberships m ON m.project_id=r.project_id AND m.id=r.id AND m.start_revision=r.start_revision
 JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision WHERE r.project_id=p.project_id AND r.pull_number=p.number AND v.complete=0)
 FROM review_current_pulls p`); err != nil {
		return err
	}
	type orderRow struct {
		project, source  string
		number, position int64
		attempt          Attempt
	}
	// Migration scans all current records under the startup deadline, in bounded
	// keyset batches. Finish each page before writing on this same connection.
	var project, source, id string
	for {
		rows, err := conn.QueryContext(ctx, currentAttemptRows+` WHERE (c.project_id,c.source,c.source_attempt_id)>(?,?,?) ORDER BY c.project_id,c.source,c.source_attempt_id LIMIT 256`, project, source, id)
		if err != nil {
			return err
		}
		var retained []orderRow
		for rows.Next() {
			var row orderRow
			if err = rows.Scan(&row.project, &row.source, &row.attempt.SourceAttemptID, &row.number, &row.position, &row.attempt.Channel, &row.attempt.StartedAt, &row.attempt.FinishedAt); err != nil {
				break
			}
			retained = append(retained, row)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, row := range retained {
			if err := insertAttemptOrder(ctx, conn, row.project, row.source, row.number, row.position, row.attempt); err != nil {
				return err
			}
			project, source, id = row.project, row.source, row.attempt.SourceAttemptID
		}
		if len(retained) < 256 {
			break
		}
	}
	return ValidateCurrentSummaries(ctx, conn)
}

// backfillCurrentReviewIndexes rebuilds derived current indexes once during an
// exclusive schema upgrade. Immutable history and current references are
// validated before any cache is discarded; their bytes are never repaired.
func backfillCurrentReviewIndexes(ctx context.Context, conn *sql.Conn) error {
	if err := ValidateHistoryReferences(ctx, conn); err != nil {
		return err
	}
	for _, table := range []string{"review_current_run_order", "review_current_attempt_order", "review_current_totals"} {
		if _, err := conn.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	if err := backfillCurrentSummaries(ctx, conn); err != nil {
		return err
	}
	if err := backfillCurrentRunOrder(ctx, conn); err != nil {
		return err
	}
	return ValidateCurrentRunOrder(ctx, conn)
}

const insertAttemptOrderSQL = `INSERT INTO review_current_attempt_order(project_id,source,source_attempt_id,pull_number,channel,sort_key,position) VALUES(?,?,?,?,?,?,?)`

func insertAttemptOrder(ctx context.Context, q currentSummarySQL, project, source string, number, position int64, attempt Attempt) error {
	key, err := attemptSortKey(attempt)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, insertAttemptOrderSQL, project, source, attempt.SourceAttemptID, number, attempt.Channel, key, position)
	return err
}

func readSummaryTotals(ctx context.Context, q HistoryQuery, project string, number int64) (summaryTotals, error) {
	var totals summaryTotals
	err := q.QueryRowContext(ctx, `SELECT run_count,attempt_count,incomplete_run_count FROM review_current_totals WHERE project_id=? AND pull_number=?`, project, number).Scan(&totals.runs, &totals.attempts, &totals.incomplete)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_pulls WHERE project_id=? AND number=?)`, project, number).Scan(&exists); err != nil {
			return totals, err
		}
		if exists {
			return totals, historyInconsistent()
		}
		return totals, nil
	}
	return totals, err
}

// Derive all touched PR counters together, while current references still
// resolve the previous snapshot. Each changed record is read once per plan.
func (w *historySnapshotWrite) prospectiveTotals(ctx context.Context, tx *sql.Tx, number int64) (summaryTotals, error) {
	totals, err := w.prepareCurrentTotals(ctx, tx)
	if err != nil {
		return summaryTotals{}, err
	}
	if total, ok := totals[number]; ok {
		return total, nil
	}
	return readSummaryTotals(ctx, w.historyRead(tx), w.project, number)
}

func validSummaryTotals(totals summaryTotals) bool {
	return totals.runs >= 0 && totals.attempts >= 0 && totals.incomplete >= 0 && totals.incomplete <= totals.runs
}

// ValidateCurrentSummaries is the read-only backup integrity boundary. It
// verifies derived values against exact current versions, never grants native
// execution authority, and is deliberately a full maintenance scan.
func ValidateCurrentSummaries(ctx context.Context, q HistoryQuery) error {
	var version int
	if err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil {
		return err
	}
	columns := summaryAttemptColumns
	if version < 8 {
		columns = summaryAttemptColumnsV7 + `,''`
	}
	queries := []string{
		`SELECT 1 FROM review_current_pulls p LEFT JOIN review_current_totals t ON t.project_id=p.project_id AND t.pull_number=p.number WHERE t.project_id IS NULL LIMIT 1`,
		`SELECT 1 FROM review_current_totals t WHERE t.run_count!=(SELECT count(*) FROM review_current_runs r WHERE r.project_id=t.project_id AND r.pull_number=t.pull_number) OR t.attempt_count!=(SELECT count(*) FROM review_current_attempts a WHERE a.project_id=t.project_id AND a.pull_number=t.pull_number) OR t.incomplete_run_count!=(SELECT count(*) FROM review_current_runs r JOIN review_run_memberships m ON m.project_id=r.project_id AND m.id=r.id AND m.start_revision=r.start_revision JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision WHERE r.project_id=t.project_id AND r.pull_number=t.pull_number AND v.complete=0) LIMIT 1`,
		`SELECT 1 FROM review_current_attempts a LEFT JOIN review_current_attempt_order o ON o.project_id=a.project_id AND o.source=a.source AND o.source_attempt_id=a.source_attempt_id WHERE o.project_id IS NULL LIMIT 1`,
	}
	for _, query := range queries {
		var found int
		err := q.QueryRowContext(ctx, query).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		return historyInconsistent()
	}
	rows, err := q.QueryContext(ctx, currentAttemptRows+` JOIN review_current_attempt_order o ON o.project_id=c.project_id AND o.source=c.source AND o.source_attempt_id=c.source_attempt_id`+` WHERE o.pull_number!=c.pull_number OR o.position!=c.position OR o.channel!=v.channel`)
	if err != nil {
		return err
	}
	bad := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if bad {
		return historyInconsistent()
	}
	rows, err = q.QueryContext(ctx, `SELECT v.started_at,v.finished_at,o.sort_key FROM review_current_attempt_order o JOIN review_current_attempts c ON c.project_id=o.project_id AND c.source=o.source AND c.source_attempt_id=o.source_attempt_id JOIN review_attempt_memberships m ON m.project_id=c.project_id AND m.source=c.source AND m.source_attempt_id=c.source_attempt_id AND m.start_revision=c.start_revision JOIN review_attempt_versions v ON v.project_id=m.project_id AND v.source=m.source AND v.source_attempt_id=m.source_attempt_id AND v.version_revision=m.version_revision`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var attempt Attempt
		var stored string
		if err := rows.Scan(&attempt.StartedAt, &attempt.FinishedAt, &stored); err != nil {
			rows.Close()
			return err
		}
		key, err := attemptSortKey(attempt)
		if err != nil {
			rows.Close()
			return err
		}
		if key != stored {
			rows.Close()
			return historyInconsistent()
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Retained compact versions must agree with the same bounded front used
	// before native publication. Finish each keyset page before its lookups so
	// maintenance works on the migration's single connection as well as a tx.
	var project string
	var number int64
	for {
		rows, err := q.QueryContext(ctx, `SELECT project_id,number FROM review_current_pulls WHERE (project_id,number)>(?,?) ORDER BY project_id,number LIMIT 128`, project, number)
		if err != nil {
			return err
		}
		type pullKey struct {
			project string
			number  int64
		}
		var page []pullKey
		for rows.Next() {
			var key pullKey
			if err = rows.Scan(&key.project, &key.number); err != nil {
				break
			}
			page = append(page, key)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, key := range page {
			if err := validateCurrentSummaryFrontColumns(ctx, q, key.project, key.number, columns); err != nil {
				return err
			}
			project, number = key.project, key.number
		}
		if len(page) < 128 {
			return nil
		}
	}
}

func (w *historySnapshotWrite) prepareCurrentTotals(ctx context.Context, tx *sql.Tx) (map[int64]summaryTotals, error) {
	if w.replacementTotals != nil {
		return w.replacementTotals, nil
	}
	deltas := map[int64]summaryTotals{}
	for _, change := range w.pending {
		if change.spec.current == historyPullRecord.current {
			number := change.keys[0].(int64)
			if _, ok := deltas[number]; !ok {
				deltas[number] = summaryTotals{}
			}
			continue
		}
		if change.spec.current != historyRunRecord.current && change.spec.current != historyAttemptRecord.current {
			continue
		}
		isRun := change.spec.current == historyRunRecord.current
		columns, join := "c.pull_number", ""
		if isRun {
			columns += ",v.complete"
			join = " JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision"
		}
		where := []string{"c.project_id=?"}
		for _, key := range change.spec.keys {
			where = append(where, "c."+key+"=?")
		}
		args := append([]any{w.project}, change.keys...)
		var oldNumber int64
		var complete bool
		targets := []any{&oldNumber}
		if isRun {
			targets = append(targets, &complete)
		}
		err := w.scan(ctx, tx, "SELECT "+columns+" FROM "+change.spec.current+" c"+join+" WHERE "+strings.Join(where, " AND "), args, targets...)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			delta := deltas[oldNumber]
			if isRun {
				delta.runs--
				if !complete {
					delta.incomplete--
				}
			} else {
				delta.attempts--
			}
			deltas[oldNumber] = delta
		}
		if change.values == nil {
			continue
		}
		newNumber, ok := change.values[1].(int64)
		if !ok {
			return nil, historyInconsistent()
		}
		delta := deltas[newNumber]
		if isRun {
			if err := w.scan(ctx, tx, `SELECT v.complete FROM review_run_memberships m JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision WHERE m.project_id=? AND m.id=? AND m.start_revision=?`, []any{w.project, change.keys[0], change.values[0]}, &complete); err != nil {
				return nil, err
			}
			delta.runs++
			if !complete {
				delta.incomplete++
			}
		} else {
			delta.attempts++
		}
		deltas[newNumber] = delta
	}
	out := make(map[int64]summaryTotals, len(deltas))
	for number, delta := range deltas {
		totals, err := readSummaryTotals(ctx, w.historyRead(tx), w.project, number)
		if err != nil {
			return nil, err
		}
		totals.runs += delta.runs
		totals.attempts += delta.attempts
		totals.incomplete += delta.incomplete
		if !validSummaryTotals(totals) {
			return nil, historyInconsistent()
		}
		out[number] = totals
	}
	return out, nil
}

func (w *historySnapshotWrite) installCurrentSummaries(ctx context.Context, tx *sql.Tx, totals map[int64]summaryTotals) error {
	for number, total := range totals {
		_, err := w.exec(ctx, tx, `INSERT INTO review_current_totals(project_id,pull_number,run_count,attempt_count,incomplete_run_count) SELECT project_id,number,?,?,? FROM review_current_pulls WHERE project_id=? AND number=? ON CONFLICT(project_id,pull_number) DO UPDATE SET run_count=excluded.run_count,attempt_count=excluded.attempt_count,incomplete_run_count=excluded.incomplete_run_count`, total.runs, total.attempts, total.incomplete, w.project, number)
		if err != nil {
			return err
		}
	}
	for _, change := range w.pending {
		if change.spec.current != historyAttemptRecord.current || change.values == nil {
			continue
		}
		var attempt Attempt
		var project, source string
		var number, position int64
		err := w.scan(ctx, tx, currentAttemptRows+` WHERE c.project_id=? AND c.source=? AND c.source_attempt_id=?`, []any{w.project, change.keys[0], change.keys[1]}, &project, &source, &attempt.SourceAttemptID, &number, &position, &attempt.Channel, &attempt.StartedAt, &attempt.FinishedAt)
		if err != nil {
			return err
		}
		key, err := attemptSortKey(attempt)
		if err != nil {
			return err
		}
		if _, err := w.exec(ctx, tx, insertAttemptOrderSQL, project, source, attempt.SourceAttemptID, number, attempt.Channel, key, position); err != nil {
			return err
		}
	}
	return nil
}

const summaryAttemptColumnsV7 = `v.source_attempt_id,v.channel,v.head_sha,v.state,v.started_at,v.finished_at,v.duration_seconds,v.source_run_id,v.trigger_id,v.provider_review_id`
const summaryAttemptColumns = summaryAttemptColumnsV7 + `,v.reason`

func (w *historySnapshotWrite) latestCurrentAttempts(ctx context.Context, tx HistoryQuery, number int64) ([]AttemptSummary, error) {
	var changed []historyCurrentChange
	for _, change := range w.pending {
		if change.spec.current == historyAttemptRecord.current {
			changed = append(changed, change)
		}
	}
	var exclusions []string
	for range changed {
		exclusions = append(exclusions, `(o.source=? AND o.source_attempt_id=?)`)
	}
	exclude := ""
	if len(exclusions) > 0 {
		exclude = " AND NOT (" + strings.Join(exclusions, " OR ") + ")"
	}
	// Each staged immutable version is independent of the channel being reduced.
	// Load it once for this PR instead of repeating the same read four times.
	type stagedCandidate struct {
		attempt  Attempt
		position int64
	}
	candidates := make([]stagedCandidate, 0, len(changed))
	for _, change := range changed {
		if change.values == nil || change.values[1] != number {
			continue
		}
		var candidate stagedCandidate
		attempt := &candidate.attempt
		err := tx.QueryRowContext(ctx, `SELECT `+summaryAttemptColumns+`,m.position FROM review_attempt_memberships m JOIN review_attempt_versions v ON v.project_id=m.project_id AND v.source=m.source AND v.source_attempt_id=m.source_attempt_id AND v.version_revision=m.version_revision WHERE m.project_id=? AND m.source=? AND m.source_attempt_id=? AND m.start_revision=?`, w.project, change.keys[0], change.keys[1], change.values[0]).Scan(&attempt.SourceAttemptID, &attempt.Channel, &attempt.HeadSHA, &attempt.State, &attempt.StartedAt, &attempt.FinishedAt, &attempt.DurationSeconds, &attempt.SourceRunID, &attempt.TriggerID, &attempt.ProviderReviewID, &attempt.Reason, &candidate.position)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	result := []AttemptSummary{}
	for _, channel := range []string{"hosted", "cli", "manual", "subagent"} {
		args := []any{w.project, number, channel}
		for _, change := range changed {
			args = append(args, change.keys...)
		}
		var best Attempt
		var bestKey string
		var bestPosition int64
		err := tx.QueryRowContext(ctx, `SELECT `+summaryAttemptColumns+`,o.sort_key,o.position FROM review_current_attempt_order o
 CROSS JOIN review_current_attempts c ON c.project_id=o.project_id AND c.source=o.source AND c.source_attempt_id=o.source_attempt_id
 CROSS JOIN review_attempt_memberships m ON m.project_id=c.project_id AND m.source=c.source AND m.source_attempt_id=c.source_attempt_id AND m.start_revision=c.start_revision
 CROSS JOIN review_attempt_versions v ON v.project_id=m.project_id AND v.source=m.source AND v.source_attempt_id=m.source_attempt_id AND v.version_revision=m.version_revision
 WHERE o.project_id=? AND o.pull_number=? AND o.channel=?`+exclude+` ORDER BY o.sort_key DESC,o.position DESC LIMIT 1`, args...).Scan(&best.SourceAttemptID, &best.Channel, &best.HeadSHA, &best.State, &best.StartedAt, &best.FinishedAt, &best.DurationSeconds, &best.SourceRunID, &best.TriggerID, &best.ProviderReviewID, &best.Reason, &bestKey, &bestPosition)
		found := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		for _, candidate := range candidates {
			attempt, position := candidate.attempt, candidate.position
			if attempt.Channel != channel {
				continue
			}
			key, err := attemptSortKey(attempt)
			if err != nil {
				return nil, err
			}
			if !found || key > bestKey || key == bestKey && position > bestPosition {
				best, bestKey, bestPosition, found = attempt, key, position, true
			}
		}
		if found {
			result = append(result, AttemptSummary(best))
		}
	}
	return result, nil
}

const currentRunOrderPageSize = 256

// CurrentRunOrderSchemaSQL defines a derived index of every current run's
// channel and eligibility metadata. It does not grant review authority.
func CurrentRunOrderSchemaSQL() []string {
	return []string{
		`CREATE TABLE review_current_run_order (
 project_id TEXT NOT NULL,
 id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 channel TEXT NOT NULL,
 outcome TEXT NOT NULL,
 finalized INTEGER NOT NULL CHECK(finalized IN(0,1)),
 run_order INTEGER NOT NULL CHECK(run_order>=0),
 PRIMARY KEY(project_id,id),
 FOREIGN KEY(project_id,id) REFERENCES review_current_runs(project_id,id) ON DELETE CASCADE
);`,
		`CREATE INDEX review_current_run_order_eligible ON review_current_run_order(project_id,pull_number,channel,run_order DESC) WHERE outcome='completed' AND finalized=1;`,
	}
}

const currentRunOrderSourceSQL = `SELECT c.project_id,c.id,c.pull_number,c.run_order,v.channel,v.outcome,v.finalized
 FROM review_current_runs c
 LEFT JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
 LEFT JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision`

const insertCurrentRunOrderSQL = `INSERT INTO review_current_run_order(project_id,id,pull_number,channel,outcome,finalized,run_order)
 SELECT c.project_id,c.id,c.pull_number,v.channel,v.outcome,v.finalized,c.run_order
 FROM review_current_runs c
 JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
 JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision
 WHERE c.project_id=? AND c.id=?`

type currentRunOrderExecutor interface {
	HistoryQuery
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// backfillCurrentRunOrder runs only inside the exclusive schema migration. It
// finishes each bounded keyset page before inserting rows on the same conn.
func backfillCurrentRunOrder(ctx context.Context, q currentRunOrderExecutor) error {
	type item struct {
		project, id, channel, outcome string
		pull, order                   int64
		finalized                     int64
	}
	var project, id string
	for {
		rows, err := q.QueryContext(ctx, currentRunOrderSourceSQL+` WHERE (c.project_id,c.id)>(?,?) ORDER BY c.project_id,c.id LIMIT ?`, project, id, currentRunOrderPageSize)
		if err != nil {
			return err
		}
		var page []item
		for rows.Next() {
			var row item
			var channel, outcome sql.NullString
			var finalized sql.NullInt64
			if err = rows.Scan(&row.project, &row.id, &row.pull, &row.order, &channel, &outcome, &finalized); err != nil {
				break
			}
			if !channel.Valid || !outcome.Valid || !finalized.Valid || (finalized.Int64 != 0 && finalized.Int64 != 1) {
				err = historyInconsistent()
				break
			}
			row.channel, row.outcome, row.finalized = channel.String, outcome.String, finalized.Int64
			page = append(page, row)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, row := range page {
			if _, err := q.ExecContext(ctx, insertCurrentRunOrderSQL, row.project, row.id); err != nil {
				return err
			}
			project, id = row.project, row.id
		}
		if len(page) < currentRunOrderPageSize {
			break
		}
	}
	return nil
}

// ValidateCurrentRunOrder is the full read-only backup integrity check for the
// derived current run-order cache. Source/current references remain authority.
func ValidateCurrentRunOrder(ctx context.Context, q HistoryQuery) error {
	var extra int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM review_current_run_order o LEFT JOIN review_current_runs c ON c.project_id=o.project_id AND c.id=o.id WHERE c.project_id IS NULL LIMIT 1`).Scan(&extra)
	if err == nil {
		return historyInconsistent()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	var project, id string
	for {
		rows, err := q.QueryContext(ctx, `SELECT c.project_id,c.id,c.pull_number,c.run_order,v.channel,v.outcome,v.finalized,
 o.project_id,o.pull_number,o.channel,o.outcome,o.finalized,o.run_order
 FROM review_current_runs c
 LEFT JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
 LEFT JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision
 LEFT JOIN review_current_run_order o ON o.project_id=c.project_id AND o.id=c.id
 WHERE (c.project_id,c.id)>(?,?) ORDER BY c.project_id,c.id LIMIT ?`, project, id, currentRunOrderPageSize)
		if err != nil {
			return err
		}
		type key struct{ project, id string }
		var page []key
		for rows.Next() {
			var current key
			var pull, order, finalized sql.NullInt64
			var channel, outcome sql.NullString
			var cachedProject, cachedChannel, cachedOutcome sql.NullString
			var cachedPull, cachedOrder, cachedFinalized sql.NullInt64
			if err := rows.Scan(&current.project, &current.id, &pull, &order, &channel, &outcome, &finalized,
				&cachedProject, &cachedPull, &cachedChannel, &cachedOutcome, &cachedFinalized, &cachedOrder); err != nil {
				rows.Close()
				return err
			}
			if !pull.Valid || !order.Valid || !channel.Valid || !outcome.Valid || !finalized.Valid ||
				!cachedProject.Valid || !cachedPull.Valid || !cachedChannel.Valid || !cachedOutcome.Valid || !cachedFinalized.Valid || !cachedOrder.Valid ||
				cachedProject.String != current.project || cachedPull.Int64 != pull.Int64 || cachedOrder.Int64 != order.Int64 ||
				cachedChannel.String != channel.String || cachedOutcome.String != outcome.String || cachedFinalized.Int64 != finalized.Int64 {
				rows.Close()
				return historyInconsistent()
			}
			page = append(page, current)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, current := range page {
			project, id = current.project, current.id
		}
		if len(page) < currentRunOrderPageSize {
			return nil
		}
	}
}
