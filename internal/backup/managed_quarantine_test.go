package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func emptyQuarantineFixture(t *testing.T) (*Manager, ownedBundle, EmptyQuarantineRequest) {
	t.Helper()
	m, b, _ := retentionCancellationFixture(t)
	path := filepath.Join(b.destination, b.id)
	manifest, _, err := m.verifyOwnedBundle(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeManagedBundle(t.Context(), path, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.DB().Exec(`UPDATE managed_backup_bundles SET state='cleanup_failed' WHERE id=?`, b.id); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	q := filepath.Join(parent, b.id)
	if err := os.Mkdir(q, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(q, "cli-exhaustion-captures"), 0700); err != nil {
		t.Fatal(err)
	}
	return m, b, EmptyQuarantineRequest{b.id, b.instance, b.hash, q, filepath.Join(parent, "receipt.json")}
}
func TestExplicitEmptyQuarantineReconciliationPreservesFilesAndRestoresRetention(t *testing.T) {
	m, b, r := emptyQuarantineFixture(t)
	before := restoreTree(t, r.QuarantinePath)
	receipt, err := m.reconcileEmptyQuarantine(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BackupBytesRemoved != 0 || receipt.State != "settled_managed_namespace_quarantine_preserved" {
		t.Fatal(receipt)
	}
	if state := retentionBundleState(t, m, b); state != "deleted" {
		t.Fatal(state)
	}
	after := restoreTree(t, r.QuarantinePath)
	if len(after) != len(before) {
		t.Fatal("quarantine modified")
	}
	if _, err := os.Stat(r.ReceiptPath); err != nil {
		t.Fatal("missing audit receipt", err)
	}
	if err := m.retain(t.Context()); err != nil {
		t.Fatal("normal retention still held", err)
	}
}
func TestEmptyQuarantineReconciliationRejectsUnprovenOrActiveState(t *testing.T) {
	for _, name := range []string{"bytes", "hash", "original", "active", "missing_keeper", "receipt_exists"} {
		t.Run(name, func(t *testing.T) {
			m, b, r := emptyQuarantineFixture(t)
			switch name {
			case "bytes":
				os.WriteFile(filepath.Join(r.QuarantinePath, "cli-exhaustion-captures", "unknown"), []byte("keep"), 0600)
			case "hash":
				r.ExpectedManifestSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "original":
				os.Mkdir(filepath.Join(b.destination, b.id), 0700)
			case "active":
				if err := m.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					r, err := loadManaged(ctx, tx)
					if err != nil {
						return err
					}
					r.Status.State = "running"
					r.Status.NextAttemptAt = ""
					r.Status.LastAttempt = &AttemptStatus{StartedAt: managedStamp(m.now()), State: "running", SourceInstanceID: m.store.InstanceID()}
					return storeManaged(ctx, tx, *r)
				}); err != nil {
					t.Fatal(err)
				}
			case "missing_keeper":
				var id string
				m.store.DB().QueryRow(`SELECT id FROM managed_backup_bundles WHERE state='retained' ORDER BY sequence DESC LIMIT 1`).Scan(&id)
				os.WriteFile(filepath.Join(b.destination, id, "manifest.json"), []byte("invalid"), 0600)
			case "receipt_exists":
				os.WriteFile(r.ReceiptPath, []byte("existing"), 0600)
			}
			if _, err := m.reconcileEmptyQuarantine(context.Background(), r); err == nil {
				t.Fatal("unsafe reconciliation accepted")
			}
			if state := retentionBundleState(t, m, b); state != "cleanup_failed" {
				t.Fatal("custody cleared", state)
			}
		})
	}
}
