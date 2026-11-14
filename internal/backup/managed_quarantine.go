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
	// Pin the scheduler generation and exact nonempty keeper set before hashing.
	// Never hold the bounded writer transaction while reading backup bytes.
	var pinnedRecord string
	var pinnedKeepers []ownedBundle
	err = m.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var e error
		pinnedRecord, pinnedKeepers, e = m.quarantineKeeperSnapshot(ctx, tx)
		return e
	})
	if err != nil {
		return receipt, err
	}
	for _, keeper := range pinnedKeepers {
		if _, _, err := m.verifyOwnedBundle(ctx, keeper); err != nil {
			return receipt, err
		}
	}

	receipt = EmptyQuarantineReceipt{1, request.BackupID, request.ExpectedInstanceID, request.ExpectedManifestSHA256, request.QuarantinePath, "prepared", 0}
	err = m.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		currentRecord, currentKeepers, err := m.quarantineKeeperSnapshot(ctx, tx)
		if err != nil || currentRecord != pinnedRecord || len(currentKeepers) != len(pinnedKeepers) {
			return invalid()
		}
		for i := range pinnedKeepers {
			if currentKeepers[i] != pinnedKeepers[i] {
				return invalid()
			}
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
	f, err := openQuarantineDirectory(path)
	if err != nil {
		return nil, invalid()
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(root, opened) {
		return nil, invalid()
	}
	entries, err := f.ReadDir(4)
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
		child, err := openQuarantineChild(f, entry.Name())
		if err != nil {
			return nil, invalid()
		}
		children, err := child.ReadDir(1)
		child.Close()
		if err == io.EOF {
			err = nil
		}
		if err != nil || len(children) != 0 {
			return nil, invalid()
		}
	}
	return root, nil
}

// quarantineKeeperSnapshot pins all configured keepers, not an independently
// re-queried count which could become empty between verification and settlement.
// The full scheduler record detects intervening completed attempts as well as
// active attempts. Retention row changes are checked independently.
func (m *Manager) quarantineKeeperSnapshot(ctx context.Context, tx *sql.Tx) (string, []ownedBundle, error) {
	record, err := loadManaged(ctx, tx)
	if err != nil || record == nil || record.Status.State == "running" {
		return "", nil, invalid()
	}
	var deleting int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM managed_backup_bundles WHERE state='deleting'").Scan(&deleting); err != nil || deleting != 0 {
		return "", nil, invalid()
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", nil, invalid()
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,destination,source_instance_id,manifest_sha256,state FROM managed_backup_bundles WHERE destination=? AND source_instance_id=? AND state='retained' ORDER BY sequence DESC LIMIT ?`, m.config.Destination, m.store.InstanceID(), m.config.Keep)
	if err != nil {
		return "", nil, unavailable()
	}
	defer rows.Close()
	var keepers []ownedBundle
	for rows.Next() {
		var b ownedBundle
		if rows.Scan(&b.id, &b.destination, &b.instance, &b.hash, &b.state) != nil {
			return "", nil, invalid()
		}
		keepers = append(keepers, b)
	}
	if rows.Err() != nil || len(keepers) == 0 {
		return "", nil, invalid()
	}
	return string(raw), keepers, nil
}

// readEmptyQuarantineOutcome resolves a lost response without repeating a write.
// A prepared receipt alone never proves settlement. A rolled-back attempt remains
// held for explicit operator resolution; this function cannot retry or clear it.
func (m *Manager) readEmptyQuarantineOutcome(ctx context.Context, request EmptyQuarantineRequest) (EmptyQuarantineReceipt, error) {
	var receipt EmptyQuarantineReceipt
	info, err := privateFile(request.ReceiptPath)
	if err != nil || info.Size() > 4096 {
		return receipt, invalid()
	}
	f, err := os.Open(request.ReceiptPath)
	if err != nil {
		return receipt, invalid()
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return receipt, invalid()
	}
	decoder := json.NewDecoder(io.LimitReader(f, 4097))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil {
		return receipt, invalid()
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return receipt, invalid()
	}
	if receipt.Version != 1 || receipt.State != "prepared" || receipt.BackupBytesRemoved != 0 ||
		receipt.BackupID != request.BackupID || receipt.InstanceID != request.ExpectedInstanceID ||
		receipt.InstanceID != m.store.InstanceID() || receipt.ManifestSHA256 != request.ExpectedManifestSHA256 ||
		receipt.QuarantinePath != request.QuarantinePath {
		return receipt, invalid()
	}
	if _, err := os.Lstat(filepath.Join(m.config.Destination, request.BackupID)); !os.IsNotExist(err) {
		return receipt, invalid()
	}
	if _, err := emptyKnownQuarantine(request.QuarantinePath); err != nil {
		return receipt, err
	}
	var instance, manifest, state, destination string
	if err := m.store.DB().QueryRowContext(ctx, "SELECT source_instance_id,manifest_sha256,state,destination FROM managed_backup_bundles WHERE id=?", request.BackupID).Scan(&instance, &manifest, &state, &destination); err != nil {
		return receipt, invalid()
	}
	if instance != receipt.InstanceID || manifest != receipt.ManifestSHA256 || destination != m.config.Destination {
		return receipt, invalid()
	}
	switch state {
	case "deleted":
		receipt.State = "settled_managed_namespace_quarantine_preserved"
	case "cleanup_failed":
		receipt.State = "prepared_not_committed_cleanup_still_held"
	default:
		return receipt, invalid()
	}
	return receipt, nil
}
