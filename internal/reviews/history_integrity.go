package reviews

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// HistoryQuery is satisfied by standard SQL databases, connections and
// transactions. Validation reads only; its caller owns the snapshot/deadline.
type HistoryQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ValidateHistoryReferences checks committed version-four membership graphs.
// Exact DDL and ordinary foreign-key checks remain the inspection owner's job.
// This does not turn imported display records into native capture authority.
func ValidateHistoryReferences(ctx context.Context, q HistoryQuery) error {
	for _, query := range historyReferenceQueries() {
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
	return nil
}

// Startup migration reuses these exact checks inside its exclusive transaction.
func historyReferenceQueries() []string {
	queries := []string{
		`SELECT 1 FROM review_snapshots s LEFT JOIN review_workspace w ON w.project_id=s.project_id WHERE w.project_id IS NULL OR s.revision>w.revision LIMIT 1`,
		`SELECT 1 FROM review_workspace w LEFT JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE s.project_id IS NULL OR s.revision!=w.revision LIMIT 1`,
	}
	equal := func(a, b string, columns []string) string {
		parts := []string{a + ".project_id=" + b + ".project_id"}
		for _, column := range columns {
			parts = append(parts, a+"."+column+"="+b+"."+column)
		}
		return strings.Join(parts, " AND ")
	}
	for _, spec := range []historyRecordSpec{historyPullRecord, historyRunRecord, historyAttemptRecord, historyRetiredRecord} {
		queries = append(queries, historyIntervalOverlapQuery(spec.memberships, spec.keys))
		// Logical-key overlap is already invalid. Any overlap at one placement
		// is therefore invalid too, without joining every historical pair.
		queries = append(queries, historyIntervalOverlapQuery(spec.memberships, spec.placement))
		membershipKey := append([]string{}, spec.keys...)
		membershipKey = append(membershipKey, "start_revision")
		membershipKey = append(membershipKey, spec.placement...)
		queries = append(queries, "SELECT 1 FROM "+spec.current+" c LEFT JOIN review_workspace w ON w.project_id=c.project_id LEFT JOIN "+spec.memberships+" m ON "+equal("c", "m", membershipKey)+" WHERE w.project_id IS NULL OR m.project_id IS NULL OR m.start_revision>w.revision OR (m.end_revision IS NOT NULL AND w.revision>=m.end_revision) LIMIT 1")
		queries = append(queries, "SELECT 1 FROM "+spec.memberships+" m JOIN review_workspace w ON w.project_id=m.project_id LEFT JOIN "+spec.current+" c ON "+equal("m", "c", membershipKey)+" WHERE m.start_revision<=w.revision AND (m.end_revision IS NULL OR w.revision<m.end_revision) AND c.project_id IS NULL LIMIT 1")
	}
	// Sweep visibility boundaries rather than joining each child to every
	// parent interval. Aggregate equal revisions first: intervals are half-open,
	// so adjacent parent versions cover a child without a gap at their boundary.
	for _, table := range []string{"review_run_memberships", "review_attempt_memberships"} {
		queries = append(queries, historyOwnerCoverageQuery(table))
	}
	return queries
}

func historyIntervalOverlapQuery(table string, keys []string) string {
	partition := strings.Join(append([]string{"project_id"}, keys...), ",")
	// ROW_NUMBER distinguishes the first row from a prior open-ended interval;
	// NULL is never replaced with a finite revision sentinel. If any intervals
	// overlap, an adjacent ordered pair overlaps, even for nested intervals.
	return `SELECT 1 FROM (
 SELECT start_revision,LAG(end_revision) OVER scope AS previous_end,ROW_NUMBER() OVER scope AS ordinal
 FROM ` + table + ` WINDOW scope AS (PARTITION BY ` + partition + ` ORDER BY start_revision)
) WHERE ordinal>1 AND (previous_end IS NULL OR start_revision<previous_end) LIMIT 1`
}

func historyOwnerCoverageQuery(table string) string {
	return `WITH events AS (
 SELECT project_id,number AS pull_number,start_revision AS revision,1 AS parent_delta,0 AS child_delta FROM review_pull_memberships
 UNION ALL SELECT project_id,number,end_revision,-1,0 FROM review_pull_memberships WHERE end_revision IS NOT NULL
 UNION ALL SELECT project_id,pull_number,start_revision,0,1 FROM ` + table + `
 UNION ALL SELECT project_id,pull_number,end_revision,0,-1 FROM ` + table + ` WHERE end_revision IS NOT NULL
), boundaries AS (
 SELECT project_id,pull_number,revision,SUM(parent_delta) AS parent_delta,SUM(child_delta) AS child_delta
 FROM events GROUP BY project_id,pull_number,revision
), visibility AS (
 SELECT SUM(parent_delta) OVER scope AS parents,SUM(child_delta) OVER scope AS children
 FROM boundaries WINDOW scope AS (PARTITION BY project_id,pull_number ORDER BY revision ROWS UNBOUNDED PRECEDING)
)
SELECT 1 FROM visibility WHERE children>0 AND parents=0 LIMIT 1`
}
