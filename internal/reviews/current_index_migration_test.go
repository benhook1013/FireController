package reviews

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// These are the actual released v10 migrations, not a current database with a
// rewound stamp. In particular the discovery index does not exist yet.
func currentIndexV10Migrations() [][]string {
	return append(reviewSchemaMigrations(), historyMigrationSQL(), CurrentSummarySchemaSQLV8(), CurrentRunOrderSchemaSQL(), pullMetadataMigrationSQL(), attemptReasonMigrationSQL(), channelRunCountsMigrationSQL(), completedChannelRunCountsMigrationSQL())
}

func currentIndexV10Fixture(t *testing.T) *apiFixture {
	t.Helper()
	f, _, _, _ := historyMigrationLegacyFixture(t)
	if err := f.store.EnsureSchemaWithBackfill(t.Context(), "reviews", currentIndexV10Migrations(), backfillCurrentReviewIndexes); err != nil {
		t.Fatal(err)
	}
	// Notification setup is independent of this retained reviews migration.
	// Install it before digesting legacy rows so the before/after boundary is exact.
	if err := EnsureNotificationSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	return f
}

func currentIndexForbidRebuild(t *testing.T, f *apiFixture) {
	t.Helper()
	for i, table := range []string{"review_current_totals", "review_current_attempt_order", "review_current_run_order"} {
		for _, operation := range []string{"INSERT", "DELETE"} {
			historyPhysicalExec(t, f, fmt.Sprintf(`CREATE TRIGGER forbid_current_index_%d_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'unexpected current index rebuild'); END`, i, operation, operation, table))
		}
	}
}

func currentIndexAssertValid(t *testing.T, f *apiFixture) {
	t.Helper()
	for _, validate := range []func(context.Context, HistoryQuery) error{ValidateHistoryReferences, ValidateCurrentSummaries, ValidateCurrentRunOrder} {
		if err := validate(t.Context(), f.store.DB()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.store.DB().QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("migration left an orphan", rows.Err())
	}
}

func TestCurrentIndexMigrationReusesValidV10Caches(t *testing.T) {
	f := currentIndexV10Fixture(t)
	currentIndexForbidRebuild(t, f)
	before := metadataLegacyRows(t, f)
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal("valid current indexes were rebuilt", err)
	}
	if after := metadataLegacyRows(t, f); !reflect.DeepEqual(after, before) {
		t.Fatal("schema-only migration changed retained rows")
	}
	var definition string
	if err := f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_run_versions_external_source' AND type='index'`).Scan(&definition); err != nil || strings.TrimSuffix(definition, ";") != strings.TrimSuffix(ExternalHostedSchemaSQL()[1], ";") {
		t.Fatal("new discovery index was not created", definition, err)
	}
	currentIndexAssertValid(t, f)
	beforeRestart := historyMigrationDatabaseDigest(t, f)
	if err := EnsureSchema(t.Context(), f.store); err != nil || historyMigrationDatabaseDigest(t, f) != beforeRestart {
		t.Fatal("repeat startup changed current indexes", err)
	}
}

func TestCurrentIndexMigrationRepairsOnlyDerivedDamage(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"missing-totals", `DELETE FROM review_current_totals`},
		{"missing-attempts", `DELETE FROM review_current_attempt_order`},
		{"missing-runs", `DELETE FROM review_current_run_order`},
		{"wrong-totals", `UPDATE review_current_totals SET run_count=run_count+1`},
		{"wrong-channel-count", `UPDATE review_current_totals SET cli_run_count=0`},
		{"wrong-attempt-time", `UPDATE review_current_attempt_order SET sort_key='wrong'`},
		{"wrong-attempt-channel", `UPDATE review_current_attempt_order SET channel='manual'`},
		{"wrong-run-order", `UPDATE review_current_run_order SET run_order=run_order+1`},
		{"wrong-run-outcome", `UPDATE review_current_run_order SET outcome='active'`},
		{"typed-total", `UPDATE review_current_totals SET run_count='not-an-integer'`},
		{"typed-channel-count", `UPDATE review_current_totals SET cli_run_count=x'31'`},
		{"typed-attempt-key", `UPDATE review_current_attempt_order SET sort_key=CAST(sort_key AS BLOB)`},
		{"typed-attempt-position", `UPDATE review_current_attempt_order SET position='not-an-integer'`},
		{"typed-run-order", `UPDATE review_current_run_order SET run_order='not-an-integer'`},
		{"typed-run-channel", `UPDATE review_current_run_order SET channel=CAST(channel AS BLOB)`},
		{"orphan-total", `INSERT INTO review_current_totals SELECT project_id,999,0,0,0,0,0,0,0 FROM review_current_totals LIMIT 1`},
		{"orphan-attempt", `INSERT INTO review_current_attempt_order SELECT project_id,source,'orphan',pull_number,channel,sort_key,position FROM review_current_attempt_order LIMIT 1`},
		{"orphan-run", `INSERT INTO review_current_run_order SELECT project_id,'orphan',pull_number,channel,outcome,finalized,run_order FROM review_current_run_order LIMIT 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := currentIndexV10Fixture(t)
			before := metadataLegacyRows(t, f)
			// Simulate a damaged offline database on one connection. Re-enable
			// enforcement before production migration, even when setup fails.
			conn, err := f.store.DB().Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF; PRAGMA ignore_check_constraints=ON; `+tc.sql)
			_, restoreErr := conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON; PRAGMA ignore_check_constraints=OFF`)
			conn.Close()
			if err != nil || restoreErr != nil {
				t.Fatal("damage fixture failed", err, restoreErr)
			}
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal("derived damage was not repaired", err)
			}
			currentIndexAssertValid(t, f)
			if after := metadataLegacyRows(t, f); !reflect.DeepEqual(after, before) {
				t.Fatal("derived repair changed retained authority or failed to restore the caches")
			}
		})
	}
}

func TestCurrentIndexMigrationRefusesAuthorityAndRollsBackDDL(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"old-membership-overlap", `UPDATE review_pull_memberships SET end_revision=9 WHERE number=8 AND start_revision=1`},
		{"retained-count", `UPDATE review_pull_versions SET run_count=run_count+1 WHERE number=7 AND version_revision=9`},
		{"retained-latest", `UPDATE review_pull_versions SET latest_attempts_json='[]' WHERE number=7 AND version_revision=9`},
		{"broken-current-reference", `DELETE FROM review_current_runs`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := currentIndexV10Fixture(t)
			historyPhysicalExec(t, f, tc.sql)
			before := historyMigrationDatabaseDigest(t, f)
			nativeErrorCode(t, EnsureSchema(t.Context(), f.store), "storage_unavailable")
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("refusal changed authority, cache rows, version metadata or discovery DDL")
			}
		})
	}
}

func TestCurrentIndexMigrationRepairFailureRollsBackAndRetries(t *testing.T) {
	f := currentIndexV10Fixture(t)
	historyPhysicalExec(t, f, `DELETE FROM review_current_run_order`)
	historyPhysicalExec(t, f, `CREATE TRIGGER refuse_index_repair BEFORE INSERT ON review_current_run_order BEGIN SELECT RAISE(ABORT,'synthetic repair failure'); END`)
	before := historyMigrationDatabaseDigest(t, f)
	nativeErrorCode(t, EnsureSchema(t.Context(), f.store), "storage_unavailable")
	if historyMigrationDatabaseDigest(t, f) != before {
		t.Fatal("failed repair leaked rows, metadata or discovery DDL")
	}
	historyPhysicalExec(t, f, `DROP TRIGGER refuse_index_repair`)
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	currentIndexAssertValid(t, f)
}

func TestCurrentIndexMigrationOlderSchemasBackfill(t *testing.T) {
	for _, version := range []int{4, 5, 6, 7, 8, 9} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f, _, _, _ := historyMigrationLegacyFixture(t)
			// v4 has no cache tables; v5 lacks run ordering; v9 has newly
			// defaulted channel counters. Every supported predecessor must
			// still derive a complete cache from the retained source graph.
			if err := f.store.EnsureSchema(t.Context(), "reviews", currentIndexV10Migrations()[:version]...); err != nil {
				t.Fatal(err)
			}
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			currentIndexAssertValid(t, f)
			assertCurrentSummaryReduction(t, f, 7)
			assertCurrentSummaryReduction(t, f, 8)
		})
	}
}

func TestCurrentIndexMigrationValidatesBeyondKeysetPages(t *testing.T) {
	for _, damage := range []string{"none", "tail-run", "tail-attempt", "tail-retained"} {
		t.Run(damage, func(t *testing.T) {
			f := newLegacyAPIFixture(t)
			tx := historyWriterTx(t, f)
			for i, project := range []string{f.project, "z-index-other"} {
				if i == 1 {
					historyMigrationExec(t, tx, `INSERT INTO projects VALUES(?,'Other',702,'owner/index-other')`, project)
				}
				snapshot := fmt.Sprintf("index-%d", i)
				historyMigrationSnapshot(t, tx, project, snapshot, 1)
				for n := 1; n <= 128+i; n++ {
					historyMigrationPull(t, tx, project, snapshot, 1, n, n-1, fmt.Sprintf("producer-%03d", n), true, false)
				}
				historyMigrationExec(t, tx, `INSERT INTO review_workspace VALUES(?,1,?)`, project, snapshot)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := f.store.EnsureSchemaWithBackfill(t.Context(), "reviews", currentIndexV10Migrations(), backfillCurrentReviewIndexes); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "none":
				currentIndexForbidRebuild(t, f)
			case "tail-run":
				historyPhysicalExec(t, f, `UPDATE review_current_run_order SET channel='manual' WHERE (project_id,id)=(SELECT project_id,id FROM review_current_run_order ORDER BY project_id DESC,id DESC LIMIT 1)`)
			case "tail-attempt":
				historyPhysicalExec(t, f, `UPDATE review_current_attempt_order SET sort_key='wrong' WHERE project_id='z-index-other' AND source='producer-129'`)
			case "tail-retained":
				historyPhysicalExec(t, f, `UPDATE review_pull_versions SET run_count=run_count+1 WHERE project_id='z-index-other' AND number=129`)
			}
			before := historyMigrationDatabaseDigest(t, f)
			err := EnsureSchema(t.Context(), f.store)
			if damage == "tail-retained" {
				nativeErrorCode(t, err, "storage_unavailable")
				if historyMigrationDatabaseDigest(t, f) != before {
					t.Fatal("tail authority refusal changed database")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				currentIndexAssertValid(t, f)
			}
		})
	}
}

func TestCurrentIndexMigrationPropagatesSQLAndCancellation(t *testing.T) {
	for _, mode := range []string{"sql-failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := currentIndexV10Fixture(t)
			if mode == "sql-failure" {
				historyPhysicalExec(t, f, `ALTER TABLE review_current_run_order RENAME COLUMN outcome TO unavailable_outcome`)
			}
			before := historyMigrationDatabaseDigest(t, f)
			var callbackErr error
			err := f.store.EnsureSchemaWithBackfill(t.Context(), "reviews", append(currentIndexV10Migrations(), ExternalHostedSchemaSQL()), func(ctx context.Context, conn *sql.Conn) error {
				if mode == "cancelled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				callbackErr = ensureCurrentReviewIndexes(ctx, conn)
				return callbackErr
			})
			if err == nil || callbackErr == nil || historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("failed read was swallowed or changed storage", err, callbackErr)
			}
			if mode == "sql-failure" && !strings.Contains(callbackErr.Error(), "no such column") {
				t.Fatal("lost actual SQL failure", callbackErr)
			}
			if mode == "cancelled" && callbackErr != context.Canceled {
				t.Fatal("lost cancellation", callbackErr)
			}
		})
	}
}
