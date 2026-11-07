package adoptionholds_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	h "github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/core"
)

type obligationQueryFailure struct {
	h.Query
	match   string
	err     error
	row     bool
	missing bool
}

func (q obligationQueryFailure) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, q.match) {
		return nil, q.err
	}
	return q.Query.QueryContext(ctx, query, args...)
}

func (q obligationQueryFailure) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if q.row && strings.Contains(query, q.match) {
		if q.missing {
			return q.Query.QueryRowContext(ctx, query+" AND 0", args...)
		}
		return q.Query.QueryRowContext(ctx, `SELECT * FROM missing_query_failure_fixture`)
	}
	return q.Query.QueryRowContext(ctx, query, args...)
}

func TestReadFactsMissingRetainedRowsRemainProtective(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "missing-retained-rows")
	for _, stage := range []string{"SELECT id,project_id,namespace", "SELECT repository_id", "SELECT github_repository_id", "SELECT source_key"} {
		q := obligationQueryFailure{Query: f.store.DB(), match: stage, row: true, missing: true}
		_, err := h.ReadFacts(t.Context(), q, "project_one", 3251)
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
			t.Fatal("missing retained row escaped protection", stage, err)
		}
	}
}

type obligationStorageCode int

func (e obligationStorageCode) Error() string { return "private driver failure" }
func (e obligationStorageCode) Code() int     { return int(e) }

func TestReadFactsPreservesOperationalErrors(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "query-errors")
	for _, stage := range []string{"SELECT obligation_id", "SELECT project_id,pull_number"} {
		for _, failure := range []error{obligationStorageCode(5), obligationStorageCode(6), context.Canceled, context.DeadlineExceeded, core.Fail("overloaded", "Safe transient failure.", 503), errors.New("private unknown SQL failure")} {
			q := obligationQueryFailure{Query: f.store.DB(), match: stage, err: failure}
			if _, err := h.ReadFacts(t.Context(), q, "project_one", 3251); !errors.Is(err, failure) {
				t.Fatalf("%s erased %T: %v", stage, failure, err)
			}
		}
	}
	for _, stage := range []string{"schema:review_adoption_obligations", "SELECT id,project_id,namespace", "SELECT repository_id", "SELECT github_repository_id", "SELECT source_key", "SELECT revision,reason"} {
		t.Run(stage, func(t *testing.T) {
			q := obligationQueryFailure{Query: f.store.DB(), match: stage, row: true}
			_, err := h.ReadFacts(t.Context(), q, "project_one", 3251)
			var code interface{ Code() int }
			if !errors.As(err, &code) || code.Code()&255 != 1 {
				t.Fatal("query failure became a retained-data hold", err)
			}
		})
	}
}

func TestReadFactsPreservesIterationFailure(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "iteration-error")
	// The first row is valid; the driver fails while advancing to the next row.
	// Remove the page suffix so the compound query orders/limits neither arm.
	q := obligationIterationFailure{Query: f.store.DB()}
	rows, err := q.QueryContext(t.Context(), `SELECT obligation_id FROM review_adoption_targets WHERE project_id=? AND pull_number=? AND obligation_id>? ORDER BY obligation_id LIMIT 64`, "project_one", 3251, "")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		rows.Close()
		t.Fatal("fixture failed before its valid row", rows.Err())
	}
	var id string
	if err := rows.Scan(&id); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if rows.Next() || rows.Err() == nil {
		rows.Close()
		t.Fatal("fixture did not fail during iteration")
	}
	rows.Close()
	_, err = h.ReadFacts(t.Context(), q, "project_one", 3251)
	var code interface{ Code() int }
	if !errors.As(err, &code) || code.Code()&255 != 1 {
		t.Fatal("partial row list concealed iteration failure", err)
	}
}

type obligationIterationFailure struct{ h.Query }

func (q obligationIterationFailure) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "SELECT obligation_id") {
		query = strings.TrimSuffix(query, " ORDER BY obligation_id LIMIT 64") + ` UNION ALL SELECT abs(-9223372036854775808)`
	}
	return q.Query.QueryContext(ctx, query, args...)
}

func TestReadFactsStoredValueCorruptionRemainsProtective(t *testing.T) {
	for _, tc := range []struct {
		name, statement string
		args            []any
	}{
		{"oversized-original", `DROP TRIGGER review_adoption_originals_immutable; UPDATE review_adoption_obligations SET original_json=?`, []any{strings.Repeat("x", 65537)}},
		{"nonnumeric-source-epoch", `UPDATE review_adoption_sources SET revision='broken'`, nil},
		{"nonnumeric-domain-version", `UPDATE metadata SET value='broken' WHERE key='schema:review_adoption_obligations'`, nil},
		{"noncanonical-domain-version", `UPDATE metadata SET value='01' WHERE key='schema:review_adoption_obligations'`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fresh(t)
			f.apply(batch(), "stored-corruption")
			f.exec(tc.statement, tc.args...)
			if tc.name == "oversized-original" {
				var body sql.NullString
				if err := f.store.DB().QueryRow(`SELECT CASE WHEN length(CAST(original_json AS BLOB))<=65536 THEN original_json END FROM review_adoption_obligations LIMIT 1`).Scan(&body); err != nil || body.Valid {
					t.Fatal("fixture did not exercise SQL NULL", body.Valid, err)
				}
			}
			_, err := h.ReadFacts(t.Context(), f.store.DB(), "project_one", 3251)
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
				t.Fatal("malformed stored value escaped protective classification", err)
			}
		})
	}
}

func TestVerifyRetainedSourceSeparatesQueryFailureAndOversizedBody(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "retained-source-boundary")
	state, err := h.ReadSource(t.Context(), f.store.DB(), "project_one", "sealed-source")
	if err != nil {
		t.Fatal(err)
	}
	q := obligationQueryFailure{Query: f.store.DB(), match: "CASE WHEN length(CAST(batch_json", row: true}
	err = h.VerifyRetainedSource(t.Context(), q, "project_one", state.Source, state.Revision)
	var code interface{ Code() int }
	if !errors.As(err, &code) || code.Code()&255 != 1 {
		t.Fatal("retained provenance query failure became corruption", err)
	}
	f.exec(`DROP TRIGGER review_adoption_imports_immutable; UPDATE review_adoption_imports SET batch_json=?`, strings.Repeat("x", 262145))
	err = h.VerifyRetainedSource(t.Context(), f.store.DB(), "project_one", state.Source, state.Revision)
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
		t.Fatal("oversized retained provenance lost protective classification", err)
	}
}
