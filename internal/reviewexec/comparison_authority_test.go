package reviewexec

import (
	"errors"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestExecutionUpgradeRefusesComparisonAuthorityBeforeMutation(t *testing.T) {
	for _, version := range []string{"3", "02", "0", "invalid", "absent"} {
		t.Run(version, func(t *testing.T) {
			f, _, _, _ := coverageFixture(t, 0, false)
			restorePreCooldownAttemptSchema(t, f)
			admissionSQL(t, f, `UPDATE metadata SET value='8' WHERE key='schema:review_execution'`)
			for _, statement := range comparisonAttemptTriggerRepairSQL() {
				admissionSQL(t, f, statement)
			}
			if version == "absent" {
				admissionSQL(t, f, `DELETE FROM metadata WHERE key='schema:review_comparison'`)
			} else {
				admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_comparison'`, version)
			}
			before := snapshotTable(t, f, "sqlite_master") + snapshotTable(t, f, "metadata") + snapshotTable(t, f, "review_comparison_proofs")
			source := comparisonSourceRows(t, f)
			err := EnsureSchema(t.Context(), f.store)
			var typed *core.Error
			code := "schema_invalid"
			if version == "3" {
				code = "schema_unsupported"
			}
			if !errors.As(err, &typed) || typed.Code != code {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if after := snapshotTable(t, f, "sqlite_master") + snapshotTable(t, f, "metadata") + snapshotTable(t, f, "review_comparison_proofs"); before != after || !reflect.DeepEqual(source, comparisonSourceRows(t, f)) {
				t.Fatal("refused comparison authority changed schema or retained rows")
			}
		})
	}
}

func TestExecutionUpgradeRefusesMissingComparisonTable(t *testing.T) {
	f, _, _, _ := coverageFixture(t, 0, false)
	comparisonPreDomain(t, f)
	admissionSQL(t, f, `INSERT INTO metadata(key,value) VALUES('schema:review_comparison','2')`)
	before := snapshotTable(t, f, "sqlite_master") + snapshotTable(t, f, "metadata")
	err := EnsureSchema(t.Context(), f.store)
	var typed *core.Error
	if !errors.As(err, &typed) || typed.Code != "schema_invalid" {
		t.Fatalf("missing retained comparison table was not refused: %v", err)
	}
	if after := snapshotTable(t, f, "sqlite_master") + snapshotTable(t, f, "metadata"); before != after {
		t.Fatal("missing comparison table refusal changed retained schema")
	}
}
