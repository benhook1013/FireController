package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type EmptyQuarantineRequest struct{ BackupID, ExpectedInstanceID, ExpectedManifestSHA256, QuarantinePath, ReceiptPath string }
type EmptyQuarantineReceipt struct {
	Version            int    `json:"version"`
	BackupID           string `json:"backup_id"`
	InstanceID         string `json:"instance_id"`
	ManifestSHA256     string `json:"manifest_sha256"`
	QuarantinePath     string `json:"quarantine_path"`
	State              string `json:"state"`
	BackupBytesRemoved int    `json:"backup_bytes_removed"`
}

// Only an explicit local operator may settle an already-removed managed bundle.
// This method never moves/unlinks a path and is never called by the scheduler.
func (m *Manager) reconcileEmptyQuarantine(ctx context.Context, request EmptyQuarantineRequest) (EmptyQuarantineReceipt, error) {
	var receipt EmptyQuarantineReceipt
	if !backupID.MatchString(request.BackupID) || request.ExpectedInstanceID != m.store.InstanceID() || !digest.MatchString(request.ExpectedManifestSHA256) || !filepath.IsAbs(request.QuarantinePath) || filepath.Clean(request.QuarantinePath) != request.QuarantinePath || filepath.Base(request.QuarantinePath) != request.BackupID || within(request.QuarantinePath, m.config.Destination) || within(m.config.Destination, request.QuarantinePath) || !filepath.IsAbs(request.ReceiptPath) || filepath.Clean(request.ReceiptPath) != request.ReceiptPath || within(request.ReceiptPath, request.QuarantinePath) || within(request.ReceiptPath, m.config.Destination) || within(request.ReceiptPath, m.dataDir) {
		return receipt, invalid()
	}
	if privateDirectory(filepath.Dir(request.ReceiptPath), false) != nil {
		return receipt, invalid()
	}
	original := filepath.Join(m.config.Destination, request.BackupID)
	if _, err := os.Lstat(original); !os.IsNotExist(err) {
		return receipt, invalid()
	}
	before, err := emptyKnownQuarantine(request.QuarantinePath)
	if err != nil {
		return receipt, err
	}
	keep, _, err := m.retentionCount(ctx)
	if err != nil || keep < 1 {
		return receipt, invalid()
	}
	if err = m.verifyKeepersCount(ctx, keep); err != nil {
		return receipt, err
	}
	receipt = EmptyQuarantineReceipt{1, request.BackupID, request.ExpectedInstanceID, request.ExpectedManifestSHA256, request.QuarantinePath, "prepared", 0}
	err = m.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		record, err := loadManaged(ctx, tx)
		if err != nil || record.Status.State == "running" {
			return invalid()
		}
		var instance, manifest, state, destination string
		if err := tx.QueryRowContext(ctx, `SELECT source_instance_id,manifest_sha256,state,destination FROM managed_backup_bundles WHERE id=?`, request.BackupID).Scan(&instance, &manifest, &state, &destination); err != nil {
			return invalid()
		}
		if instance != request.ExpectedInstanceID || manifest != request.ExpectedManifestSHA256 || state != "cleanup_failed" || destination != m.config.Destination {
			return invalid()
		}
		if _, err := os.Lstat(original); !os.IsNotExist(err) {
			return invalid()
		}
		after, err := emptyKnownQuarantine(request.QuarantinePath)
		if err != nil || !os.SameFile(before, after) {
			return invalid()
		}
		body, err := json.Marshal(receipt)
		if err != nil {
			return invalid()
		}
		f, err := os.OpenFile(request.ReceiptPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return invalid()
		}
		_, writeErr := f.Write(append(body, '\n'))
		syncErr := f.Sync()
		closeErr := f.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return unavailable()
		}
		if err := syncDirectory(filepath.Dir(request.ReceiptPath)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE managed_backup_bundles SET state='deleted' WHERE id=? AND state='cleanup_failed' AND manifest_sha256=? AND source_instance_id=? AND destination=?`, request.BackupID, request.ExpectedManifestSHA256, request.ExpectedInstanceID, m.config.Destination)
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
		return receipt, err
	}
	// Durable prepared receipt plus terminal ledger are jointly authoritative. A
	// failed response must be resolved by reading both, not repeating the action.
	receipt.State = "settled_managed_namespace_quarantine_preserved"
	return receipt, nil
}
func emptyKnownQuarantine(path string) (os.FileInfo, error) {
	if privateDirectory(path, false) != nil {
		return nil, invalid()
	}
	root, err := os.Lstat(path)
	if err != nil {
		return nil, invalid()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, invalid()
	}
	entries, err := f.ReadDir(4)
	f.Close()
	if err == io.EOF {
		err = nil
	}
	if err != nil || len(entries) > 3 {
		return nil, invalid()
	}
	for _, entry := range entries {
		if !strings.Contains("|review-captures|hosted-review-captures|cli-exhaustion-captures|", "|"+entry.Name()+"|") || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, invalid()
		}
		f, err := os.Open(filepath.Join(path, entry.Name()))
		if err != nil {
			return nil, invalid()
		}
		children, err := f.ReadDir(1)
		f.Close()
		if err == io.EOF {
			err = nil
		}
		if err != nil || len(children) != 0 {
			return nil, invalid()
		}
	}
	return root, nil
}
