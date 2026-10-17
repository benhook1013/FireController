package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func retentionCancellationFixture(t *testing.T) (*Manager, ownedBundle, map[string]string) {
	t.Helper()
	f, m := newManagedFixture(t, 2)
	if reports := runManaged(t, m, 2); len(reports) != 0 {
		t.Fatal(reports)
	}
	var b ownedBundle
	if err := f.store.DB().QueryRow(`SELECT id,destination,source_instance_id,manifest_sha256,state FROM managed_backup_bundles ORDER BY sequence LIMIT 1`).Scan(&b.id, &b.destination, &b.instance, &b.hash, &b.state); err != nil {
		t.Fatal(err)
	}
	m.config.Keep = 1
	return m, b, restoreTree(t, filepath.Join(b.destination, b.id))
}
func retentionBundleState(t *testing.T, m *Manager, b ownedBundle) string {
	t.Helper()
	var state string
	if err := m.store.DB().QueryRow(`SELECT state FROM managed_backup_bundles WHERE id=?`, b.id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
func TestManagedRetentionCancellationBeforeFirstUnlinkCanRetry(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			m, b, before := retentionCancellationFixture(t)
			original := m.remove
			m.remove = func(ctx context.Context, path string, manifest Manifest) error {
				if retentionBundleState(t, m, b) != "deleting" {
					t.Fatal("deletion was not admitted")
				}
				stopped, cancel := context.WithCancel(ctx)
				if deadline {
					cancel()
					stopped, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				}
				cancel()
				return removeManagedBundle(stopped, path, manifest)
			}
			if err := m.retain(t.Context()); err == nil {
				t.Fatal("cancellation not reported")
			}
			if state := retentionBundleState(t, m, b); state != "retained" {
				t.Fatalf("untouched bundle permanently poisoned: %s", state)
			}
			if !reflect.DeepEqual(before, restoreTree(t, filepath.Join(b.destination, b.id))) {
				t.Fatal("initial cancellation changed files")
			}
			m.remove = original
			if err := m.retain(t.Context()); err != nil {
				t.Fatal("next maintenance could not retry", err)
			}
			if state := retentionBundleState(t, m, b); state != "deleted" {
				t.Fatal(state)
			}
		})
	}
}
func TestManagedRetentionCancelledOpaqueRemovalStillHolds(t *testing.T) {
	for _, sameCode := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			t.Run(map[bool]string{false: "opaque error", true: "partial unlink"}[partial]+map[bool]string{false: "/raw", true: "/same public code"}[sameCode], func(t *testing.T) {
				m, b, before := retentionCancellationFixture(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				m.remove = func(_ context.Context, path string, _ Manifest) error {
					if partial {
						if err := os.Remove(filepath.Join(path, "manifest.sha256")); err != nil {
							t.Fatal(err)
						}
					}
					cancel()
					if sameCode {
						return unavailable()
					}
					return errors.New("unclassified removal result")
				}
				if err := m.retain(ctx); err == nil {
					t.Fatal("removal failure not reported")
				}
				if state := retentionBundleState(t, m, b); state != "cleanup_failed" {
					t.Fatal("unknown removal was retried", state)
				}
				if !partial && !reflect.DeepEqual(before, restoreTree(t, filepath.Join(b.destination, b.id))) {
					t.Fatal("opaque failure changed files")
				}
				m.remove = func(context.Context, string, Manifest) error { t.Fatal("failed deletion retried"); return nil }
				if err := m.retain(t.Context()); err == nil {
					t.Fatal("cleanup hold missing")
				}
			})
		}
	}
}

// The context cancels at a real I/O/transaction boundary without sleeping or
// replacing verification/removal with a fake successful implementation.
type retentionBoundaryContext struct {
	context.Context
	cancel   context.CancelFunc
	boundary func() bool
}

func (c *retentionBoundaryContext) Err() error {
	if c.Context.Err() == nil && c.boundary() {
		c.cancel()
	}
	return c.Context.Err()
}
func TestManagedRetentionCancelledVerificationBoundaryDefersTamperJudgment(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		for _, tamper := range []bool{false, true} {
			t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline]+map[bool]string{false: "/valid retry", true: "/tamper next attempt"}[tamper], func(t *testing.T) {
				m, b, before := retentionCancellationFixture(t)
				rowsBefore := backupRows(t, m.store.DB())
				ctx, cancel := context.WithCancel(t.Context())
				if deadline {
					cancel()
					ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				}
				cancel()
				// Exercise the actual verification boundary with an already stopped
				// context; this does not inject cancellation partway through hashing.
				manifest, info, err := m.verifyOwnedBundle(ctx, b)
				if !errors.Is(err, errManagedVerificationInterrupted) || info != nil || !reflect.DeepEqual(manifest, Manifest{}) {
					t.Fatal("interrupted verification claimed validity", manifest, info, err)
				}
				path := filepath.Join(b.destination, b.id)
				if !reflect.DeepEqual(rowsBefore, backupRows(t, m.store.DB())) || !reflect.DeepEqual(before, restoreTree(t, path)) {
					t.Fatal("cancelled verification changed rows or files")
				}
				if tamper {
					if err := os.WriteFile(filepath.Join(path, "manifest.json"), []byte("tampered"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				err = m.retain(t.Context())
				if tamper {
					if err == nil || retentionBundleState(t, m, b) != "cleanup_failed" {
						t.Fatal("fresh verification ignored tamper", err)
					}
				} else if err != nil || retentionBundleState(t, m, b) != "deleted" {
					t.Fatal("untouched candidate not retryable", err)
				}
			})
		}
	}
}
func TestManagedRetentionCancellationAfterAdmissionBeforeRemoval(t *testing.T) {
	m, b, before := retentionCancellationFixture(t)
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &retentionBoundaryContext{Context: base, cancel: cancel}
	ctx.boundary = func() bool { return retentionBundleState(t, m, b) == "deleting" }
	m.remove = func(context.Context, string, Manifest) error {
		t.Fatal("cancelled admission invoked removal")
		return nil
	}
	if err := m.retain(ctx); err == nil {
		t.Fatal("cancellation not reported")
	}
	if ctx.Err() == nil || retentionBundleState(t, m, b) != "retained" {
		t.Fatal("untouched admission not reverted")
	}
	if !reflect.DeepEqual(before, restoreTree(t, filepath.Join(b.destination, b.id))) {
		t.Fatal("cancelled admission changed files")
	}
	m.remove = removeManagedBundle
	if err := m.retain(t.Context()); err != nil {
		t.Fatal("next maintenance could not retry", err)
	}
}
func TestManagedRetentionProductionRemovalCancellationAfterFirstUnlinkHolds(t *testing.T) {
	m, b, _ := retentionCancellationFixture(t)
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &retentionBoundaryContext{Context: base, cancel: cancel}
	path := filepath.Join(b.destination, b.id)
	ctx.boundary = func() bool { _, err := os.Lstat(filepath.Join(path, "manifest.sha256")); return os.IsNotExist(err) }
	if err := m.retain(ctx); err == nil {
		t.Fatal("partial cancellation not reported")
	}
	if ctx.Err() == nil || retentionBundleState(t, m, b) != "cleanup_failed" {
		t.Fatal("actual first unlink was treated as untouched")
	}
	if _, err := os.Stat(filepath.Join(path, "firecontroller.sqlite")); err != nil {
		t.Fatal("partial remnants lost", err)
	}
	m.remove = func(context.Context, string, Manifest) error { t.Fatal("partial cleanup retried"); return nil }
	if err := m.retain(t.Context()); err == nil {
		t.Fatal("partial cleanup hold missing")
	}
}

func TestManagedRetentionUntouchedSettlementRequiresExactLedgerIdentity(t *testing.T) {
	m, b, before := retentionCancellationFixture(t)
	m.remove = func(ctx context.Context, path string, manifest Manifest) error {
		if _, err := m.store.DB().Exec(`UPDATE managed_backup_bundles SET manifest_sha256=? WHERE id=?`, strings.Repeat("a", 64), b.id); err != nil {
			t.Fatal(err)
		}
		stopped, cancel := context.WithCancel(ctx)
		cancel()
		return removeManagedBundle(stopped, path, manifest)
	}
	if err := m.retain(t.Context()); err == nil {
		t.Fatal("changed ledger identity was silently reset")
	}
	if retentionBundleState(t, m, b) != "deleting" {
		t.Fatal("identity mismatch reset to retryable")
	}
	if !reflect.DeepEqual(before, restoreTree(t, filepath.Join(b.destination, b.id))) {
		t.Fatal("identity mismatch changed files")
	}
}

func TestManagedRetentionNewDestinationPreservesOldCleanupHold(t *testing.T) {
	m, b, _ := retentionCancellationFixture(t)
	path := filepath.Join(b.destination, b.id)
	if err := os.WriteFile(filepath.Join(path, "manifest.json"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.retain(t.Context()); err == nil || retentionBundleState(t, m, b) != "cleanup_failed" {
		t.Fatal("old destination lacks genuine hold", err)
	}
	oldTree := restoreTree(t, b.destination)
	oldLedger := func() []string {
		rows, err := m.store.DB().Query(`SELECT sequence,id,destination,source_instance_id,manifest_sha256,state FROM managed_backup_bundles WHERE destination=? ORDER BY sequence`, b.destination)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var records []string
		for rows.Next() {
			var seq int64
			var old ownedBundle
			if err := rows.Scan(&seq, &old.id, &old.destination, &old.instance, &old.hash, &old.state); err != nil {
				t.Fatal(err)
			}
			records = append(records, fmt.Sprintf("%d/%+v", seq, old))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return records
	}
	before := oldLedger()
	cfg := m.config
	cfg.Destination = filepath.Join(t.TempDir(), "new-private-destination")
	replacement, err := NewManager(m.store, m.dataDir, cfg)
	if err != nil {
		t.Fatal("new destination refused", err)
	}
	if reports := runManaged(t, replacement, 2); len(reports) != 0 {
		t.Fatal("old destination hold poisoned new destination", reports)
	}
	var retained, deleted int
	if err := m.store.DB().QueryRow(`SELECT sum(state='retained'),sum(state='deleted') FROM managed_backup_bundles WHERE destination=? AND source_instance_id=?`, cfg.Destination, m.store.InstanceID()).Scan(&retained, &deleted); err != nil || retained != 1 || deleted != 1 {
		t.Fatal("new destination did not retain/prune owned bundles", retained, deleted, err)
	}
	if !reflect.DeepEqual(before, oldLedger()) || !reflect.DeepEqual(oldTree, restoreTree(t, b.destination)) {
		t.Fatal("destination selection altered old held ledger or bytes")
	}
	if err := m.retain(t.Context()); err == nil || retentionBundleState(t, m, b) != "cleanup_failed" {
		t.Fatal("old cleanup hold was cleared", err)
	}
}
