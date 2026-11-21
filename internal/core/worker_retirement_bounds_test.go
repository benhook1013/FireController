package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// Observe the actual SQL result, not just the final rejection: checking in Go
// after Scan would still materialize an unbounded attacker-controlled value.
type retirementBoundedReader struct {
	db   *sql.DB
	t    *testing.T
	rows int
}

func (r *retirementBoundedReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var at, actor string
		var worker sql.NullString
		if err := rows.Scan(&at, &actor, &worker); err != nil {
			r.t.Fatal(err)
		}
		if len(at) > 30 || len(actor) > 256 {
			r.t.Fatalf("SQL returned unbounded values: timestamp=%d actor=%d", len(at), len(actor))
		}
		r.rows++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return r.db.QueryContext(ctx, query, args...)
}

func TestWorkerRetirementSnapshotByteBounds(t *testing.T) {
	const maximumTimestamp = "9999-12-31T23:59:59.999999999Z"
	if len(maximumTimestamp) != 30 {
		t.Fatal("incorrect canonical UTC timestamp bound")
	}
	for _, tc := range []struct {
		name, at, actor string
		valid           bool
	}{
		{"maximum", maximumTimestamp, strings.Repeat("a", 256), true},
		{"maximum_utf8", maximumTimestamp, strings.Repeat("é", 128), true},
		{"whole_second", "2026-11-01T00:00:00Z", "actor", true},
		{"timestamp_over_boundary", maximumTimestamp + "0", "actor", false},
		{"timestamp_large", strings.Repeat("t", 1<<20), "actor", false},
		{"actor_over_boundary", maximumTimestamp, strings.Repeat("a", 257), false},
		{"actor_utf8_over_boundary", maximumTimestamp, strings.Repeat("é", 129), false},
		{"actor_large", maximumTimestamp, strings.Repeat("a", 1<<20), false},
		{"actor_nul_large", maximumTimestamp, "a\x00" + strings.Repeat("a", 1<<20), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.Context(), filepath.Join(t.TempDir(), "store"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			_, err = store.DB().ExecContext(t.Context(), `INSERT INTO projects(id,name,github_repository_id,repository) VALUES('project','Project',1,'owner/repo'); INSERT INTO workers(project_id,id,name) VALUES('project','valid','Valid'),('project','test','Test'); INSERT INTO worker_retirements VALUES('project','valid','2026-11-01T00:00:00Z','actor')`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.DB().ExecContext(t.Context(), `INSERT INTO worker_retirements VALUES('project','test',?,?)`, tc.at, tc.actor)
			if err != nil {
				t.Fatal(err)
			}
			reader := &retirementBoundedReader{db: store.DB(), t: t}
			err = ValidateWorkerRetirementsSnapshot(t.Context(), reader)
			if reader.rows != 2 {
				t.Fatal("SQL silently discarded retirement rows", reader.rows)
			}
			if tc.valid {
				if err != nil {
					t.Fatal("valid boundary rejected", err)
				}
			} else {
				receiptCode(t, err, "storage_unavailable")
			}
		})
	}
}
