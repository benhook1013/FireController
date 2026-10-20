package backup

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUnboundedRoundsCanonicalBackupRetainsExplicitNoMaximum(t *testing.T) {
	f := newAdoptionBackupFixture(t)
	bundle := restoreBundle(t, f.restoreFixture)
	checked, err := Inspect(t.Context(), bundle.Path)
	if err != nil || checked.State != "verified" {
		t.Fatal("canonical unbounded bundle refused", checked, err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var minimum, maximum, flag, used, dry, closed, changes int64
	var body string
	if err = db.QueryRow(`SELECT minimum,maximum,maximum_unbounded,used,dry,closed FROM review_round_controls`).Scan(&minimum, &maximum, &flag, &used, &dry, &closed); err != nil || minimum != 1001 || maximum != 0 || flag != 1 || used != 0 || dry != 2 || closed != 1 {
		t.Fatal("unbounded minimum-only rule changed", minimum, maximum, flag, used, dry, closed, err)
	}
	if err = db.QueryRow(`SELECT prepared_json FROM review_round_adoptions`).Scan(&body); err != nil || !strings.Contains(body, `"maximum_unbounded":true`) || !strings.Contains(body, `"snapshot_sha256":"`+f.source.SnapshotSHA256+`"`) {
		t.Fatal("prepared explicit bound/source linkage lost", err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM review_round_changes`).Scan(&changes); err != nil || changes != 2 {
		t.Fatal("policy change history lost", changes, err)
	}
	if restoreRows(t, db, `SELECT count(*) FROM review_round_completions`)[0][0] != int64(0) {
		t.Fatal("imported dry streak became native completion credit")
	}
}
func TestUnboundedRoundsResealedFalseCeilingRefusedBeforeRestore(t *testing.T) {
	f := newAdoptionBackupFixture(t)
	bundle := restoreBundle(t, f.restoreFixture)
	mutateBundleSQL(t, bundle.Path, `UPDATE review_round_adoptions SET prepared_json=json_set(prepared_json,'$.maximum_unbounded',json('false'))`)
	assertCurrentSummaryInventoryRefused(t, bundle, true)
}
