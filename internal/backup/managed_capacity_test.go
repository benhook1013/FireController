package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedByteBudgetPrunesBelowCountLimitAndPreservesForeignFiles(t *testing.T) {
	f, m := newManagedFixture(t, 24)
	if reports := runManaged(t, m, 3); len(reports) != 0 {
		t.Fatal(reports)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	size, err := bundleDiskBytes(t.Context(), status.LastSuccess.Path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := m.config
	cfg.MaxBytes = size + size/2
	m, err = NewManager(f.store, f.dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(cfg.Destination, "unowned.txt")
	if err = os.WriteFile(foreign, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.retain(t.Context()); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state='retained'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal(retained, err)
	}
	if _, err = Inspect(t.Context(), status.LastSuccess.Path); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(foreign); err != nil || string(body) != "keep me" {
		t.Fatal(err)
	}
}

func TestManagedByteBudgetKeepsLatestWhenOneCopyExceedsBudget(t *testing.T) {
	f, m := newManagedFixture(t, 24)
	if reports := runManaged(t, m, 2); len(reports) != 0 {
		t.Fatal(reports)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := m.config
	cfg.MaxBytes = 1
	m, err = NewManager(f.store, f.dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.retain(t.Context()); err == nil {
		t.Fatal("oversized safety floor reported success")
	}
	if _, err = Inspect(t.Context(), status.LastSuccess.Path); err != nil {
		t.Fatal("latest usable copy deleted", err)
	}
	var retained int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state='retained'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal(retained, err)
	}
}

func TestManagedByteBudgetDoesNotPruneWithDamagedNewestKeeper(t *testing.T) {
	f, m := newManagedFixture(t, 24)
	if reports := runManaged(t, m, 2); len(reports) != 0 {
		t.Fatal(reports)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	size, err := bundleDiskBytes(t.Context(), status.LastSuccess.Path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := m.config
	cfg.MaxBytes = size + size/2
	m, err = NewManager(f.store, f.dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(status.LastSuccess.Path, "manifest.json"), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.retain(t.Context()); err == nil {
		t.Fatal("damaged keeper permitted pruning")
	}
	var deleted int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state='deleted'`).Scan(&deleted); err != nil || deleted != 0 {
		t.Fatal(deleted, err)
	}
}

func TestManagedUnresolvedRetentionStopsGrowthOnlyAtBudget(t *testing.T) {
	for _, state := range []string{"deleting", "cleanup_failed"} {
		t.Run(state, func(t *testing.T) {
			f, m := newManagedFixture(t, 24)
			if reports := runManaged(t, m, 3); len(reports) != 0 {
				t.Fatal(reports)
			}
			if _, err := f.store.DB().Exec(`UPDATE managed_backup_bundles SET state=? WHERE sequence=(SELECT min(sequence) FROM managed_backup_bundles)`, state); err != nil {
				t.Fatal(err)
			}
			if err := m.admitSnapshotGrowth(t.Context()); err != nil {
				t.Fatal("ample budget unexpectedly blocked", err)
			}
			before, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(m.config.Destination)
			if err != nil {
				t.Fatal(err)
			}
			cfg := m.config
			cfg.MaxBytes = 1
			m, err = NewManager(f.store, f.dir, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.attempt(t.Context()); err == nil {
				t.Fatal("exhausted retention budget admitted new snapshot")
			}
			after, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			if after.LastSuccess.ID != before.LastSuccess.ID || after.LastAttempt.Reason != "backup_retention_failed" {
				t.Fatalf("after success=%+v attempt=%+v before=%+v", after.LastSuccess, after.LastAttempt, before.LastSuccess)
			}
			current, err := os.ReadDir(m.config.Destination)
			if err != nil || len(current) != len(entries) {
				t.Fatal("snapshot files changed", err)
			}
			var held int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state IN ('deleting','cleanup_failed')`).Scan(&held); err != nil || held != 1 {
				t.Fatal("custody changed", held, err)
			}
			if _, err := Inspect(t.Context(), before.LastSuccess.Path); err != nil {
				t.Fatal("latest backup damaged", err)
			}
		})
	}
}
