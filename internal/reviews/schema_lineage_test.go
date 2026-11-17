package reviews

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"reflect"
	"testing"
)

// Build each real predecessor from its migration sequence, not by merely
// changing a current database's version stamp.
func lineageFixture(t *testing.T, lineage string) *apiFixture {
	t.Helper()
	f, _, _, _ := historyMigrationLegacyFixture(t)
	migrations := append(reviewSchemaMigrations(), historyMigrationSQL(), CurrentSummarySchemaSQLV8(), CurrentRunOrderSchemaSQL(), pullMetadataMigrationSQL(), attemptReasonMigrationSQL(), channelRunCountsMigrationSQL())
	switch lineage {
	case "main10":
		migrations = append(migrations, completedChannelRunCountsMigrationSQL())
	case "slice11":
		migrations = append(migrations, completedChannelRunCountsMigrationSQL(), ExternalHostedSchemaSQL())
	case "integrated10":
		migrations = append(migrations, ExternalHostedSchemaSQL())
	case "integrated11":
		migrations = append(migrations, ExternalHostedSchemaSQL(), deliverySelectionMigrationSQL())
	case "integrated12":
		migrations = append(migrations, ExternalHostedSchemaSQL(), deliverySelectionMigrationSQL(), completedChannelRunCountsMigrationSQL())
	default:
		t.Fatal("unknown fixture lineage")
	}
	backfill := func(ctx context.Context, conn *sql.Conn) error {
		if err := backfillCurrentReviewIndexes(ctx, conn); err != nil {
			return err
		}
		if lineage == "main10" || lineage == "slice11" {
			for _, q := range completedChannelRunCountsMigrationSQL() {
				if _, err := conn.ExecContext(ctx, q); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := f.store.EnsureSchemaWithBackfill(t.Context(), "reviews", migrations, backfill); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReviewSchemaLineagesConvergeWithoutChangingRetainedData(t *testing.T) {
	for _, lineage := range []string{"main10", "slice11", "integrated10", "integrated11", "integrated12"} {
		t.Run(lineage, func(t *testing.T) {
			f := lineageFixture(t, lineage)
			wantUnselected := 0
			if lineage == "integrated11" || lineage == "integrated12" {
				if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET delivery_selected=0 WHERE rowid=(SELECT min(rowid) FROM review_pull_versions)`); err != nil {
					t.Fatal(err)
				}
				wantUnselected = 1
			}
			wantFrontiers := 0
			if lineage != "main10" {
				if _, err := f.store.DB().Exec(`INSERT INTO review_hosted_discovery_state VALUES(?,701,7,'2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, f.project); err != nil {
					t.Fatal(err)
				}
				wantFrontiers = 1
			}
			if lineage == "integrated12" {
				if err := EnsureNotificationSchema(t.Context(), f.store); err != nil {
					t.Fatal(err)
				}
			}
			var metadataBefore string
			const metadataQuery = `SELECT group_concat(hex(metadata_json),'|') FROM (SELECT metadata_json FROM review_pull_versions ORDER BY project_id,number,version_revision)`
			if err := f.store.DB().QueryRow(metadataQuery).Scan(&metadataBefore); err != nil {
				t.Fatal(err)
			}
			currentDigest := historyMigrationDatabaseDigest(t, f)
			before := metadataLegacyRows(t, f)
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			if lineage == "integrated12" && currentDigest != historyMigrationDatabaseDigest(t, f) {
				t.Fatal("current integrated-v12 startup was not an exact no-op")
			}
			if after := metadataLegacyRows(t, f); !reflect.DeepEqual(before, after) {
				t.Fatal("retained data changed")
			}
			var metadataAfter string
			if err := f.store.DB().QueryRow(metadataQuery).Scan(&metadataAfter); err != nil || metadataAfter != metadataBefore {
				t.Fatal("retained metadata bodies changed", err)
			}
			var version string
			if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "12" {
				t.Fatal(version, err)
			}
			var selected int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_pull_versions WHERE delivery_selected!=1`).Scan(&selected); err != nil || selected != wantUnselected {
				t.Fatal("Delivery prerequisite missing or existing identity selection changed", selected, err)
			}
			var frontiers int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_hosted_discovery_state WHERE cutoff_at='2026-01-01T00:00:00Z' AND initialized_at='2026-01-02T00:00:00Z'`).Scan(&frontiers); err != nil || frontiers != wantFrontiers {
				t.Fatal("discovery frontier changed", frontiers, err)
			}
			for _, validate := range []func(context.Context, HistoryQuery) error{ValidateHistoryReferences, ValidateCurrentSummaries, ValidateCurrentRunOrder, ValidateExternalHostedFrontiers} {
				if err := validate(t.Context(), f.store.DB()); err != nil {
					t.Fatal(err)
				}
			}
			digest := historyMigrationDatabaseDigest(t, f)
			if err := EnsureSchema(t.Context(), f.store); err != nil || historyMigrationDatabaseDigest(t, f) != digest {
				t.Fatal("repeat startup changed current data", err)
			}
		})
	}
}

func TestReviewSchemaLineageRepairRollsBackWithPendingMigration(t *testing.T) {
	for _, lineage := range []string{"main10", "slice11"} {
		t.Run(lineage, func(t *testing.T) {
			f := lineageFixture(t, lineage)
			if _, err := f.store.DB().Exec(`CREATE TRIGGER refuse_lineage_count_update BEFORE UPDATE ON review_current_totals BEGIN SELECT RAISE(ABORT,'fixture count migration failure'); END`); err != nil {
				t.Fatal(err)
			}
			before := historyMigrationDatabaseDigest(t, f)
			if err := EnsureSchema(t.Context(), f.store); err == nil {
				t.Fatal("expected pending migration failure")
			}
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("compatibility repair or version metadata escaped rollback")
			}
		})
	}
}

func TestReviewSchemaLineageUnknownShapesRefuseWithoutWrites(t *testing.T) {
	cases := []struct{ name, lineage, sql string }{
		{"partial-discovery", "integrated10", `DROP INDEX review_run_versions_external_source`},
		{"missing-discovery", "slice11", `DROP INDEX review_run_versions_external_source; DROP TABLE review_hosted_discovery_state`},
		{"changed-discovery", "integrated11", `DROP INDEX review_run_versions_external_source; CREATE INDEX review_run_versions_external_source ON review_run_versions(project_id,head_sha)`},
		{"changed-view", "main10", `DROP VIEW review_pulls; CREATE VIEW review_pulls AS SELECT 1 AS invalid`},
		{"current-missing-discovery", "integrated12", `DROP INDEX review_run_versions_external_source`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := lineageFixture(t, tc.lineage)
			if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { _, err := tx.ExecContext(ctx, tc.sql); return err }); err != nil {
				t.Fatal(err)
			}
			before := historyMigrationDatabaseDigest(t, f)
			err := EnsureSchema(t.Context(), f.store)
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "schema_incompatible" {
				t.Fatal("unknown lineage accepted or misclassified", err)
			}
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("refusal changed database")
			}
		})
	}
	t.Run("current-missing-Delivery", func(t *testing.T) {
		f := lineageFixture(t, "integrated12")
		if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			for _, q := range []string{`DROP VIEW review_pulls`, `ALTER TABLE review_pull_versions DROP COLUMN delivery_selected`, HistoryReadViewSQLV10()["review_pulls"]} {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		before := historyMigrationDatabaseDigest(t, f)
		if err := EnsureSchema(t.Context(), f.store); err == nil || historyMigrationDatabaseDigest(t, f) != before {
			t.Fatal("current schema guessed/repaired as an old lineage", err)
		}
	})
}
