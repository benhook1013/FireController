package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

func manifestHash(m Manifest) (string, error) {
	body, err := canonicalManifest(m)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:]), nil
}

type ownedBundle struct{ id, destination, instance, hash, state string }

// These private signals distinguish interrupted read-only verification and a
// positively untouched removal from opaque failures. They retain safe errors.
var errManagedVerificationInterrupted = unavailable()
var errManagedRemovalNotStarted = unavailable()

func (m *Manager) retain(ctx context.Context) error {
	if checkContext(ctx) != nil {
		return unavailable()
	}
	// Failed/possibly partial cleanups are not an automatic deletion retry.
	var failed int
	if err := m.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_backup_bundles WHERE destination=? AND source_instance_id=? AND state IN ('deleting','cleanup_failed'))`, m.config.Destination, m.store.InstanceID()).Scan(&failed); err != nil {
		return unavailable()
	}
	if failed != 0 {
		return invalid()
	}
	// Without an excess owned copy, retention has nothing to delete and makes
	// no claim about the present bytes of the protected bundles.
	keep, oversized, err := m.retentionCount(ctx)
	if err != nil {
		return err
	}
	var candidate int
	if err := m.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_backup_bundles WHERE destination=? AND source_instance_id=? AND state='retained' ORDER BY sequence DESC LIMIT 1 OFFSET ?)`, m.config.Destination, m.store.InstanceID(), keep).Scan(&candidate); err != nil {
		return unavailable()
	}
	if candidate == 0 {
		if oversized {
			return invalid()
		}
		return nil
	}
	// Verify the retained safety floor once per maintenance pass, before any
	// older copy can be removed. Ledger ownership alone is not a usable backup.
	if err := m.verifyKeepersCount(ctx, keep); err != nil {
		return err
	}
	for {
		if checkContext(ctx) != nil {
			return unavailable()
		}
		var b ownedBundle
		err := m.store.DB().QueryRowContext(ctx, `SELECT id,destination,source_instance_id,manifest_sha256,state FROM managed_backup_bundles WHERE destination=? AND source_instance_id=? AND state='retained' ORDER BY sequence DESC LIMIT 1 OFFSET ?`, m.config.Destination, m.store.InstanceID(), keep).Scan(&b.id, &b.destination, &b.instance, &b.hash, &b.state)
		if errors.Is(err, sql.ErrNoRows) {
			if oversized {
				return invalid()
			}
			return nil
		}
		if err != nil {
			return unavailable()
		}
		manifest, before, err := m.verifyOwnedBundle(ctx, b)
		if err != nil {
			if errors.Is(err, errManagedVerificationInterrupted) {
				return err
			}
			return m.cleanupFailed(ctx, b.id)
		}
		path := filepath.Join(b.destination, b.id)
		err = m.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			result, err := tx.ExecContext(ctx, `UPDATE managed_backup_bundles SET state='deleting' WHERE id=? AND destination=? AND source_instance_id=? AND manifest_sha256=? AND state='retained'`, b.id, b.destination, b.instance, b.hash)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				return invalid()
			}
			return nil
		})
		if err != nil {
			return err
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, current) {
			return m.cleanupFailed(ctx, b.id)
		}
		if checkContext(ctx) != nil {
			return m.retainUntouched(b)
		}
		if err = m.remove(ctx, path, manifest); err != nil {
			if errors.Is(err, errManagedRemovalNotStarted) {
				return m.retainUntouched(b)
			}
			return m.cleanupFailed(ctx, b.id)
		}
		if err = syncDirectory(b.destination); err != nil {
			return m.cleanupFailed(ctx, b.id)
		}
		// The filesystem deletion is complete. Cancellation cannot undo it;
		// settle its ledger fact with an independent, bounded deadline.
		settle, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), managedSettlementTimeout)
		err = m.store.BackgroundWrite(settle, func(ctx context.Context, tx *sql.Tx) error {
			result, e := tx.ExecContext(ctx, `UPDATE managed_backup_bundles SET state='deleted' WHERE id=? AND state='deleting'`, b.id)
			if e != nil {
				return e
			}
			n, e := result.RowsAffected()
			if e != nil || n != 1 {
				return invalid()
			}
			return nil
		})
		cancelSettle()
		if err != nil {
			return err
		}
	}
}
func (m *Manager) verifyKeepers(ctx context.Context) error {
	return m.verifyKeepersCount(ctx, m.config.Keep)
}
func (m *Manager) verifyKeepersCount(ctx context.Context, keep int) error {
	rows, err := m.store.DB().QueryContext(ctx, `SELECT id,destination,source_instance_id,manifest_sha256,state FROM managed_backup_bundles WHERE destination=? AND source_instance_id=? AND state='retained' ORDER BY sequence DESC LIMIT ?`, m.config.Destination, m.store.InstanceID(), keep)
	if err != nil {
		return unavailable()
	}
	var keepers []ownedBundle
	for rows.Next() {
		var b ownedBundle
		if rows.Scan(&b.id, &b.destination, &b.instance, &b.hash, &b.state) != nil {
			rows.Close()
			return invalid()
		}
		keepers = append(keepers, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return unavailable()
	}
	for _, b := range keepers {
		if _, _, err := m.verifyOwnedBundle(ctx, b); err != nil {
			return err
		}
	}
	return nil
}
func (m *Manager) verifyOwnedBundle(ctx context.Context, b ownedBundle) (Manifest, os.FileInfo, error) {
	var zero Manifest
	if !backupID.MatchString(b.id) || !digest.MatchString(b.hash) || b.destination != m.config.Destination || b.instance != m.store.InstanceID() || b.state != "retained" {
		return zero, nil, invalid()
	}
	path := filepath.Join(b.destination, b.id)
	before, err := os.Lstat(path)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return zero, nil, invalid()
	}
	manifest, err := inspectManifest(ctx, path)
	if err != nil {
		// Inspection cannot unlink. Cancellation can obscure a validation
		// failure inside hashing; defer that judgment, never deletion checks.
		// The next attempt must inspect the entire bundle again.
		if ctx.Err() != nil {
			return zero, nil, errManagedVerificationInterrupted
		}
		return zero, nil, err
	}
	hash, err := manifestHash(manifest)
	if err != nil || manifest.ID != b.id || manifest.SourceInstanceID != b.instance || hash != b.hash {
		return zero, nil, invalid()
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		return zero, nil, invalid()
	}
	return manifest, before, nil
}

// Revert only this exact admitted candidate, when no unlink has
// started. A failed settlement leaves deleting for startup's strict hold.
func (m *Manager) retainUntouched(b ownedBundle) error {
	ctx, cancel := context.WithTimeout(context.Background(), managedSettlementTimeout)
	defer cancel()
	if err := m.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE managed_backup_bundles SET state='retained' WHERE id=? AND destination=? AND source_instance_id=? AND manifest_sha256=? AND state='deleting'`, b.id, b.destination, b.instance, b.hash)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return invalid()
		}
		return nil
	}); err != nil {
		return err
	}
	return unavailable()
}

func (m *Manager) cleanupFailed(_ context.Context, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), managedSettlementTimeout)
	defer cancel()
	if err := m.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE managed_backup_bundles SET state='cleanup_failed' WHERE id=? AND state IN ('retained','deleting')`, id)
		return err
	}); err != nil {
		return err
	}
	return invalid()
}

// Delete only the verified inventory, not a recursive tree that might now hold
// unrelated additions. Every completed unlink leaves an inspectably incomplete
// bundle if cancellation or a later unlink fails.
func removeManagedBundle(ctx context.Context, path string, manifest Manifest) error {
	if checkContext(ctx) != nil {
		// No os.Remove has been attempted. Never return this signal after
		// the first unlink, even when a later error is cancellation.
		return errManagedRemovalNotStarted
	}
	if err := os.Remove(filepath.Join(path, "manifest.sha256")); err != nil {
		return unavailable()
	}
	for _, artifact := range manifest.Artifacts {
		if checkContext(ctx) != nil {
			return unavailable()
		}
		if err := os.Remove(filepath.Join(path, filepath.FromSlash(artifact.Path))); err != nil {
			return unavailable()
		}
	}
	for _, dir := range []string{"review-captures", "hosted-review-captures"} {
		if checkContext(ctx) != nil {
			return unavailable()
		}
		if err := os.Remove(filepath.Join(path, dir)); err != nil && !os.IsNotExist(err) {
			return unavailable()
		}
	}
	for _, file := range []string{"firecontroller.sqlite", "manifest.json"} {
		if checkContext(ctx) != nil {
			return unavailable()
		}
		if err := os.Remove(filepath.Join(path, file)); err != nil {
			return unavailable()
		}
	}
	if checkContext(ctx) != nil {
		return unavailable()
	}
	if err := os.Remove(path); err != nil {
		return unavailable()
	}
	return nil
}
