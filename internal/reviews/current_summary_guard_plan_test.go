package reviews

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

// Repeated integrity checks must prepare this bounded winner lookup even when
// there is no selected attempt. Keep preparation measurable independently of
// fixture construction, historical row counts and statement execution.
func BenchmarkCurrentSummaryAttemptPreparation(b *testing.B) {
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(b.TempDir(), "private"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	if err := EnsureSchema(ctx, store); err != nil {
		b.Fatal(err)
	}
	query := currentSummaryAttemptQuery(summaryAttemptColumns)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		statement, err := store.DB().PrepareContext(ctx, query)
		if err != nil {
			b.Fatal(err)
		}
		if err := statement.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCurrentSummaryAttemptLookupUsesBoundedIndexes(t *testing.T) {
	f := currentSummaryGuardFixture(t)
	for _, columns := range []string{summaryAttemptColumns} {
		rows, err := f.store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+currentSummaryAttemptQuery(columns), f.project, 1, "cli")
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan.WriteString(detail)
			plan.WriteByte('\n')
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		text := plan.String()
		for alias, identity := range map[string]string{
			"o": "(project_id=? AND pull_number=? AND channel=?)",
			"c": "(project_id=? AND source=? AND source_attempt_id=?)",
			"m": "(project_id=? AND source=? AND source_attempt_id=? AND start_revision=?)",
			"v": "(project_id=? AND source=? AND source_attempt_id=? AND version_revision=?)",
		} {
			found := false
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "SEARCH "+alias+" USING INDEX ") && strings.Contains(line, identity) && (alias != "o" || strings.Contains(line, "review_current_attempt_order_latest")) {
					found = true
				}
			}
			if !found {
				t.Fatalf("winner query lost full indexed identity for %s %s:\n%s", alias, identity, text)
			}
		}
		if strings.Contains(text, "SCAN ") || strings.Contains(text, "TEMP B-TREE") {
			t.Fatalf("winner query scans or sorts retained history:\n%s", text)
		}
	}
}

// The canonical identity columns use matching default-BINARY TEXT or INTEGER
// declarations. Exercise canonical values and SQL-storable malformed classes:
// unary + must not change equality after these column affinities are applied.
func TestCurrentSummaryJoinKeysPreserveSQLiteStoredValueComparison(t *testing.T) {
	f := newAPIFixture(t)
	for _, declaration := range []string{"TEXT COLLATE BINARY", "INTEGER"} {
		tx := historyWriterTx(t, f)
		if _, err := tx.ExecContext(t.Context(), "CREATE TEMP TABLE summary_key_left(value "+declaration+"); CREATE TEMP TABLE summary_key_right(value "+declaration+")"); err != nil {
			t.Fatal(err)
		}
		values := []any{nil, int64(0), int64(1), int64(-1), 1.5, "", "0", "01", "1.0", "not-an-integer", "original-A", "original-a", []byte("0"), []byte("original-A")}
		for _, value := range values {
			for _, table := range []string{"summary_key_left", "summary_key_right"} {
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO "+table+" VALUES(?)", value); err != nil {
					t.Fatal(err)
				}
			}
		}
		var mismatches int
		if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM summary_key_left a CROSS JOIN summary_key_right b WHERE (a.value=b.value) IS NOT (a.value=+b.value)`).Scan(&mismatches); err != nil || mismatches != 0 {
			t.Fatal("unary + changed canonical/malformed key comparison", declaration, mismatches, err)
		}
		var caseInsensitive int
		if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM summary_key_left a CROSS JOIN summary_key_right b WHERE typeof(a.value)='text' AND typeof(b.value)='text' AND a.value='original-A' AND b.value='original-a' AND a.value=+b.value`).Scan(&caseInsensitive); err != nil || caseInsensitive != 0 {
			t.Fatal("key comparison lost BINARY collation", declaration, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}
