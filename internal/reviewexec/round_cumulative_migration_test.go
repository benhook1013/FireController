package reviewexec

import (
	"reflect"
	"testing"
)

func TestRoundCumulativeV3UpgradePreservesLegacyRecordsUntilExplicitEdit(t *testing.T) {
	f := roundFixture(t)
	seedLegacyRoundGrant(t, f, 2, "cli", 0, 0, 4, "predecessor")
	a, id := roundCapture(t, f, 0, false)
	roundFinalize(t, f, a, id, 0, false)
	original := roundRead(t, f, 2, "cli")
	var originalHistory string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes`).Scan(&originalHistory); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`ALTER TABLE review_round_controls RENAME TO fixture_controls_v4`, RoundSchemaV3SQL()[0],
		`INSERT INTO review_round_controls(` + roundColumns + `) SELECT ` + roundColumns + ` FROM fixture_controls_v4`,
		`DROP TABLE fixture_controls_v4`, `UPDATE metadata SET value='3' WHERE key='schema:review_rounds'`,
	} {
		if _, err := f.store.DB().Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	adoptionValid(t, f)
	before := roundTables(t, f)
	var ddl string
	if err := f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_round_controls'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_round4 BEFORE UPDATE OF value ON metadata WHEN NEW.key='schema:review_rounds' AND NEW.value='4' BEGIN SELECT RAISE(ABORT,'owned fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRoundSchema(t.Context(), f.store); err == nil {
		t.Fatal("migration failure did not roll back")
	}
	var afterDDL, version string
	if err := f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_round_controls'`).Scan(&afterDDL); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_rounds'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if afterDDL != ddl || version != "3" || roundTables(t, f) != before {
		t.Fatal("failed upgrade altered prior schema or records")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER fail_round4`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if got := roundRead(t, f, 2, "cli"); !reflect.DeepEqual(got, original) {
		t.Fatal("migration changed live allowance", got, original)
	}
	var afterHistory string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes`).Scan(&afterHistory); err != nil || afterHistory != originalHistory {
		t.Fatal("migration rewrote legacy grant", err)
	}
	record, err := readRoundRecord(t.Context(), f.store.DB(), attemptProject, 2, "cli")
	if err != nil || record.cumulative {
		t.Fatal("upgrade changed legacy semantics", err)
	}
	adoptionValid(t, f)
	roundGrant(t, f, 2, "cli", 1, 0, 2, "explicit-edit")
	record, err = readRoundRecord(t.Context(), f.store.DB(), attemptProject, 2, "cli")
	if err != nil || !record.cumulative || record.Epoch != 1 || record.Used != 1 || record.Dry != 1 || record.Maximum != 3 {
		t.Fatal("explicit edit failed format2 transition", record, err)
	}
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes WHERE revision=1`).Scan(&afterHistory); err != nil || afterHistory != originalHistory {
		t.Fatal("explicit edit rewrote old history", err)
	}
	adoptionValid(t, f)
}

func TestRoundCumulativeHistoryRejectsAlteredCutoffAndUnknownFormat(t *testing.T) {
	for _, kind := range []string{"cutoff", "format", "binding"} {
		t.Run(kind, func(t *testing.T) {
			f := roundFixture(t)
			roundGrant(t, f, 2, "cli", 0, 0, 4, "first")
			a, id := roundCapture(t, f, 0, false)
			roundFinalize(t, f, a, id, 0, false)
			roundGrant(t, f, 2, "cli", 1, 0, 3, "limits")
			adoptionValid(t, f)
			query := `UPDATE review_round_changes SET body_json=json_set(body_json,'$.control.used',2) WHERE revision=2`
			if kind == "format" {
				query = `UPDATE review_round_changes SET body_json=json_set(body_json,'$.format',3) WHERE revision=2`
			}
			if kind == "binding" {
				query = `UPDATE review_round_operation_bindings SET grant_revision=2`
			}
			if _, err := f.store.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err == nil {
				t.Fatal("altered cumulative history accepted", kind)
			}
		})
	}
}
