package adoptionholds_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	h "github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/core"
)

type snapshotReadFailure struct {
	h.Query
	match      string
	skip, hits int
	failureCtx context.Context
	missing    bool
}

func (q *snapshotReadFailure) fail(query string) bool {
	if !strings.Contains(query, q.match) {
		return false
	}
	q.hits++
	return q.hits > q.skip
}

func (q *snapshotReadFailure) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if q.fail(query) {
		if q.missing {
			return q.Query.QueryRowContext(ctx, query+" AND 0", args...)
		}
		if q.failureCtx != nil {
			return q.Query.QueryRowContext(q.failureCtx, query, args...)
		}
		return q.Query.QueryRowContext(ctx, `SELECT * FROM missing_snapshot_failure_fixture`)
	}
	return q.Query.QueryRowContext(ctx, query, args...)
}

func (q *snapshotReadFailure) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if q.fail(query) {
		if q.failureCtx != nil {
			return q.Query.QueryContext(q.failureCtx, query, args...)
		}
		return q.Query.QueryContext(ctx, `SELECT * FROM missing_snapshot_failure_fixture`)
	}
	return q.Query.QueryContext(ctx, query, args...)
}

func TestSnapshotTraversalPreservesOperationalErrors(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "snapshot-errors")
	f.validate()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelDeadline := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	for _, stage := range []struct {
		name, query string
		skip        int
	}{
		{"source", "SELECT repository_id", 0},
		{"project", "SELECT github_repository_id", 0},
		{"import", "SELECT batch_json,batch_sha256,actor_id", 0},
		{"obligation-id", "SELECT id FROM review_adoption_obligations", 0},
		{"source-detail", "SELECT id,project_id,namespace,source_revision", 0},
		{"source-targets", "SELECT project_id,pull_number", 0},
		{"source-resolution", "SELECT revision,reason,evidence_json", 0},
		// Each original is checked during source traversal before the separate
		// obligation traversal. Exercise both callers of the retained reader.
		{"snapshot-detail", "SELECT id,project_id,namespace,source_revision", len(batch().Obligations)},
		{"snapshot-targets", "SELECT project_id,pull_number", len(batch().Obligations)},
	} {
		for _, failure := range []struct {
			name string
			ctx  context.Context
			want error
		}{
			{"driver", nil, nil},
			{"canceled", canceled, context.Canceled},
			{"deadline", expired, context.DeadlineExceeded},
		} {
			t.Run(stage.name+"/"+failure.name, func(t *testing.T) {
				q := &snapshotReadFailure{Query: f.store.DB(), match: stage.query, skip: stage.skip, failureCtx: failure.ctx}
				err := h.ValidateSnapshot(t.Context(), q)
				if q.hits != stage.skip+1 {
					t.Fatalf("did not reach the intended read: hits=%d, error=%v", q.hits, err)
				}
				if failure.want != nil {
					if !errors.Is(err, failure.want) {
						t.Fatalf("lost operational error %v: %v", failure.want, err)
					}
				} else {
					var code interface{ Code() int }
					if !errors.As(err, &code) || code.Code()&255 != 1 {
						t.Fatal("lost SQLite query failure", err)
					}
				}
			})
		}
	}
}

func TestSnapshotTraversalMissingRowsRemainUnavailable(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "snapshot-missing")
	for _, stage := range []struct {
		query string
		skip  int
	}{
		{"SELECT repository_id", 0},
		{"SELECT github_repository_id", 0},
		{"SELECT batch_json,batch_sha256,actor_id", 0},
		{"SELECT id FROM review_adoption_obligations", 0},
		{"SELECT id,project_id,namespace,source_revision", 0},
		{"SELECT id,project_id,namespace,source_revision", len(batch().Obligations)},
	} {
		q := &snapshotReadFailure{Query: f.store.DB(), match: stage.query, skip: stage.skip, missing: true}
		err := h.ValidateSnapshot(t.Context(), q)
		var failure *core.Error
		if q.hits != stage.skip+1 || !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
			t.Fatalf("missing row escaped protective classification (%s, skip=%d): %v", stage.query, stage.skip, err)
		}
	}
}

type snapshotSchemaIterationFailure struct{ h.Query }

func (q snapshotSchemaIterationFailure) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "SELECT name,CASE WHEN length(CAST(sql AS BLOB))") {
		// SQLite emits the valid schema rows, then fails while stepping into
		// the second arm, rather than failing QueryContext itself.
		query += ` UNION ALL SELECT 'iteration_failure',abs(-9223372036854775808)`
	}
	return q.Query.QueryContext(ctx, query, args...)
}

func TestSnapshotSchemaIterationPreservesOperationalError(t *testing.T) {
	f := fresh(t)
	q := snapshotSchemaIterationFailure{Query: f.store.DB()}
	rows, err := q.QueryContext(t.Context(), `SELECT name,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE name GLOB 'review_adoption_*' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		count++
	}
	err = rows.Err()
	rows.Close()
	if count != len(h.SchemaSQL()) || err == nil {
		t.Fatalf("fixture did not emit the full schema before iteration failed: rows=%d, error=%v", count, err)
	}
	err = h.ValidateSnapshot(t.Context(), q)
	var code interface{ Code() int }
	if !errors.As(err, &code) || code.Code()&255 != 1 {
		t.Fatal("schema iteration failure became retained corruption", err)
	}
}

func TestSnapshotInvalidRetainedGraphRemainsUnavailable(t *testing.T) {
	for _, corruption := range []string{"repository-changed", "invalid-project", "invalid-namespace", "oversized-schema", "malformed-import", "missing-import"} {
		t.Run(corruption, func(t *testing.T) {
			f := fresh(t)
			f.apply(batch(), "snapshot-corruption")
			f.validate()
			switch corruption {
			case "repository-changed":
				f.exec(`UPDATE projects SET github_repository_id=99 WHERE id='project_one'`)
			case "invalid-project":
				f.exec(`INSERT INTO projects VALUES('invalid.project','Invalid',19,'owner/invalid')`)
				f.exec(`INSERT INTO review_adoption_sources VALUES('invalid.project','sealed-source',19,?,1)`, strings.Repeat("a", 64))
			case "invalid-namespace":
				f.exec(`INSERT INTO review_adoption_sources VALUES('project_one',?,17,?,1)`, "invalid\nnamespace", strings.Repeat("a", 64))
			case "oversized-schema":
				f.exec(`CREATE TRIGGER review_adoption_oversized AFTER INSERT ON review_adoption_sources BEGIN SELECT 1 /*` + strings.Repeat("x", 16385) + `*/; END`)
			case "malformed-import":
				f.exec(`DROP TRIGGER review_adoption_imports_immutable`)
				f.exec(`UPDATE review_adoption_imports SET batch_json='{}'`)
				for _, definition := range h.SchemaSQL() {
					if strings.HasPrefix(definition, "CREATE TRIGGER review_adoption_imports_immutable ") {
						f.exec(definition)
					}
				}
			case "missing-import":
				f.exec(`UPDATE review_adoption_sources SET revision=2`)
			}
			var failure *core.Error
			if err := h.ValidateSnapshot(t.Context(), f.store.DB()); !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
				t.Fatal("invalid retained graph escaped protective classification", err)
			}
		})
	}
}
