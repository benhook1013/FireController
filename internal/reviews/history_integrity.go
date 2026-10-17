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
		queries = append(queries, "SELECT 1 FROM "+spec.memberships+" a JOIN "+spec.memberships+" b ON "+equal("a", "b", spec.keys)+" AND a.start_revision<b.start_revision WHERE a.end_revision IS NULL OR b.start_revision<a.end_revision LIMIT 1")
		// Unique current ordering alone cannot detect a collision confined to
		// older closed intervals. Reject those collisions without enumerating
		// every snapshot and reconstructing its whole record set.
		queries = append(queries, "SELECT 1 FROM "+spec.memberships+" a JOIN "+spec.memberships+" b ON "+equal("a", "b", spec.placement)+" WHERE NOT ("+equal("a", "b", spec.keys)+") AND (a.end_revision IS NULL OR b.start_revision<a.end_revision) AND (b.end_revision IS NULL OR a.start_revision<b.end_revision) LIMIT 1")
		membershipKey := append([]string{}, spec.keys...)
		membershipKey = append(membershipKey, "start_revision")
		membershipKey = append(membershipKey, spec.placement...)
		queries = append(queries, "SELECT 1 FROM "+spec.current+" c LEFT JOIN review_workspace w ON w.project_id=c.project_id LEFT JOIN "+spec.memberships+" m ON "+equal("c", "m", membershipKey)+" WHERE w.project_id IS NULL OR m.project_id IS NULL OR m.start_revision>w.revision OR (m.end_revision IS NOT NULL AND w.revision>=m.end_revision) LIMIT 1")
		queries = append(queries, "SELECT 1 FROM "+spec.memberships+" m JOIN review_workspace w ON w.project_id=m.project_id LEFT JOIN "+spec.current+" c ON "+equal("m", "c", membershipKey)+" WHERE m.start_revision<=w.revision AND (m.end_revision IS NULL OR w.revision<m.end_revision) AND c.project_id IS NULL LIMIT 1")
	}
	// Runs and attempts must retain the owning pull throughout visibility,
	// including old gaps hidden by a currently valid parent. It is enough to
	// inspect child starts and parent ends: those are the only boundaries where
	// a previously covered child can lose its owning pull.
	for _, table := range []string{"review_run_memberships", "review_attempt_memberships"} {
		queries = append(queries, "SELECT 1 FROM "+table+" c WHERE NOT EXISTS(SELECT 1 FROM review_pull_memberships p WHERE p.project_id=c.project_id AND p.number=c.pull_number AND p.start_revision<=c.start_revision AND (p.end_revision IS NULL OR c.start_revision<p.end_revision)) LIMIT 1")
		queries = append(queries, "SELECT 1 FROM review_pull_memberships p WHERE p.end_revision IS NOT NULL AND EXISTS(SELECT 1 FROM "+table+" c WHERE c.project_id=p.project_id AND c.pull_number=p.number AND c.start_revision<=p.end_revision AND (c.end_revision IS NULL OR p.end_revision<c.end_revision)) AND NOT EXISTS(SELECT 1 FROM review_pull_memberships n WHERE n.project_id=p.project_id AND n.number=p.number AND n.start_revision<=p.end_revision AND (n.end_revision IS NULL OR p.end_revision<n.end_revision)) LIMIT 1")
	}
	return queries
}
