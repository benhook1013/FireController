package backup

import (
	"database/sql"
	"os"
	"reflect"
	"testing"
)

func TestManagedHealthUsesExistingSnapshotWithoutDestinationIO(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	original, err := ReadStoreStatus(t.Context(), f.store)
	if err != nil {
		t.Fatal(err)
	}
	before := backupRows(t, f.store.DB())
	moved := m.config.Destination + "-not-mounted"
	if err := os.Rename(m.config.Destination, moved); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(moved, m.config.Destination)
	actual, err := ReadStoreStatus(t.Context(), f.store)
	if err != nil || !reflect.DeepEqual(actual, original) {
		t.Fatal(actual, err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	record, err := ReadHealthRecord(t.Context(), tx)
	tx.Rollback()
	if err != nil || !record.Configured || record.LastSnapshotAt != original.LastSuccess.SnapshotFinishedAt || record.SuccessInstance != f.store.InstanceID() || record.Interval != m.config.Interval {
		t.Fatal(record, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("status read changed backup or coordination rows")
	}
}
