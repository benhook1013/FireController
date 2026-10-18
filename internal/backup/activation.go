package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type ActivationRequest struct{ ExpectedInstanceID, FencedPredecessorID, AdminTokenFile string }
type RecoveryStatus struct {
	InstanceID       string `json:"instance_id"`
	SourceInstanceID string `json:"source_instance_id"`
	BundleID         string `json:"bundle_id"`
	State            string `json:"state"`
	ProvidersHeld    bool   `json:"providers_held"`
	CredentialID     string `json:"credential_id"`
	ActivatedAt      string `json:"activated_at"`
}
type ActivationSummary struct {
	InstanceID       string `json:"instance_id"`
	SourceInstanceID string `json:"source_instance_id"`
	BundleID         string `json:"bundle_id"`
	State            string `json:"state"`
	ProvidersHeld    bool   `json:"providers_held"`
	CredentialID     string `json:"credential_id"`
	ActivatedAt      string `json:"activated_at"`
}

var activationCredentialID = regexp.MustCompile(`^credential_[0-9a-f]{32}$`)
var activationPrincipalID = regexp.MustCompile(`^principal_[0-9a-f]{32}$`)

func activationConflict() error {
	return core.Fail("restore_conflict", "The activation identity, fence attestation or selected credential conflicts with retained recovery facts.", 409)
}

// ReadRecoveryStatus reads local recovery facts only. Historical activation does
// not assert that its administrator credential still permits access.
func ReadRecoveryStatus(ctx context.Context, store *core.Store) (RecoveryStatus, error) {
	if checkContext(ctx) != nil || store == nil {
		return RecoveryStatus{}, unavailable()
	}
	tx, err := store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RecoveryStatus{}, unavailable()
	}
	defer tx.Rollback()
	status, err := readRecoveryStatus(ctx, tx)
	if err == nil && status.InstanceID != store.InstanceID() {
		return RecoveryStatus{}, invalid()
	}
	return status, err
}
func readRecoveryStatus(ctx context.Context, db sqlReader) (RecoveryStatus, error) {
	var status RecoveryStatus
	var signing string
	if db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='instance_id'`).Scan(&status.InstanceID) != nil || !instanceID.MatchString(status.InstanceID) || db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='signing_key'`).Scan(&signing) != nil || !digest.MatchString(signing) {
		return RecoveryStatus{}, invalid()
	}
	fields, err := readRestoreFields(ctx, db)
	if err != nil {
		return RecoveryStatus{}, err
	}
	var actual int64
	if db.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM events`).Scan(&actual) != nil {
		return RecoveryStatus{}, invalid()
	}
	status, sequence, err := recoveryStatusFields(ctx, db, status.InstanceID, fields, actual)
	if err != nil {
		return RecoveryStatus{}, err
	}
	if err = validateRecoveryHistory(ctx, db, status, sequence); err != nil {
		return RecoveryStatus{}, err
	}
	return status, nil
}

// recoveryStatusFields validates one generation without traversing history.
// Its upper sequence is the containing generation's recovery point.
func recoveryStatusFields(ctx context.Context, db sqlReader, identity string, fields map[string]string, actual int64) (RecoveryStatus, int64, error) {
	status := RecoveryStatus{InstanceID: identity}
	if !instanceID.MatchString(identity) {
		return RecoveryStatus{}, 0, invalid()
	}
	if len(fields) == 0 {
		status.State = "ordinary"
		return status, actual, nil
	}
	status.State = fields["restore:state"]
	status.SourceInstanceID = fields["restore:source_instance_id"]
	status.BundleID = fields["restore:bundle_id"]
	status.ProvidersHeld = true
	start, startOK := canonicalTime(fields["restore:snapshot_started_at"])
	finish, finishOK := canonicalTime(fields["restore:snapshot_finished_at"])
	sequence, err := strconv.ParseInt(fields["restore:event_sequence"], 10, 64)
	if !instanceID.MatchString(status.SourceInstanceID) || status.SourceInstanceID == identity || !backupID.MatchString(status.BundleID) || !startOK || !finishOK || finish.Before(start) || err != nil || sequence < 0 || strconv.FormatInt(sequence, 10) != fields["restore:event_sequence"] || actual < sequence {
		return RecoveryStatus{}, 0, invalid()
	}
	switch status.State {
	case "inactive":
		var active int64
		if len(fields) != 6 || actual != sequence || db.QueryRowContext(ctx, `SELECT count(*) FROM credentials WHERE revoked<>1`).Scan(&active) != nil || active != 0 {
			return RecoveryStatus{}, 0, invalid()
		}
	case "coordination_active":
		status.CredentialID = fields["restore:activation_credential_id"]
		status.ActivatedAt = fields["restore:activated_at"]
		_, atOK := canonicalTime(status.ActivatedAt)
		var principal, hash string
		var admin, revoked int
		if len(fields) != 9 || fields["restore:provider_state"] != "held" || !activationCredentialID.MatchString(status.CredentialID) || !atOK || db.QueryRowContext(ctx, `SELECT principal_id,token_sha256,is_admin,revoked FROM credentials WHERE id=?`, status.CredentialID).Scan(&principal, &hash, &admin, &revoked) != nil || !activationPrincipalID.MatchString(principal) || !digest.MatchString(hash) || admin != 1 || revoked < 0 || revoked > 1 {
			return RecoveryStatus{}, 0, invalid()
		}
	default:
		return RecoveryStatus{}, 0, invalid()
	}
	return status, sequence, nil
}

func activationTokenHash(path string) (string, error) {
	if path == "" || restoreParent(filepath.Dir(path)) != nil {
		return "", invalid()
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && before.Mode().Perm()&0077 != 0 || before.Size() < 32 || before.Size() > 258 {
		return "", invalid()
	}
	f, err := os.Open(path)
	if err != nil {
		return "", unavailable()
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", invalid()
	}
	raw, err := io.ReadAll(io.LimitReader(f, 259))
	if err != nil || len(raw) > 258 {
		return "", invalid()
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if len(token) < 32 || len(token) > 256 {
		return "", invalid()
	}
	for _, b := range []byte(token) {
		if b < 33 || b > 126 {
			return "", invalid()
		}
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), nil
}
func checkActivationRequest(ctx context.Context, db sqlReader, status RecoveryStatus, in ActivationRequest, hash string) error {
	if status.State == "ordinary" {
		return invalid()
	}
	if status.InstanceID != in.ExpectedInstanceID || status.SourceInstanceID != in.FencedPredecessorID {
		return activationConflict()
	}
	var credential string
	err := db.QueryRowContext(ctx, `SELECT id FROM credentials WHERE token_sha256=?`, hash).Scan(&credential)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return unavailable()
	}
	if status.State == "coordination_active" {
		if err != nil || credential != status.CredentialID {
			return activationConflict()
		}
		return nil
	}
	if err == nil {
		return activationConflict()
	}
	return nil
}

// Activate admits local coordination only, after an explicit operator attestation
// that the predecessor is fenced. It never verifies that external fence or runs
// provider reconciliation, catalog loading, workers or capture interpretation.
func Activate(ctx context.Context, dataDir string, in ActivationRequest) (ActivationSummary, error) {
	var zero ActivationSummary
	if checkContext(ctx) != nil {
		return zero, unavailable()
	}
	if !instanceID.MatchString(in.ExpectedInstanceID) || !instanceID.MatchString(in.FencedPredecessorID) || in.ExpectedInstanceID == in.FencedPredecessorID || strings.TrimSpace(dataDir) == "" {
		return zero, invalid()
	}
	if err := CheckDataDirectory(dataDir); err != nil {
		return zero, err
	}
	if privateDirectory(dataDir, false) != nil {
		return zero, invalid()
	}
	hash, err := activationTokenHash(in.AdminTokenFile)
	if err != nil {
		return zero, err
	}
	db, err := openDatabase(ctx, filepath.Join(dataDir, "firecontroller.sqlite"), false)
	if err != nil {
		return zero, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		return zero, unavailable()
	}
	if _, err = validateSchema(ctx, tx, true); err == nil {
		err = activationIntegrity(ctx, tx)
	}
	var status RecoveryStatus
	if err == nil {
		status, err = readRecoveryStatus(ctx, tx)
	}
	if err == nil {
		err = checkActivationRequest(ctx, tx, status, in, hash)
	}
	tx.Rollback()
	db.Close()
	if err != nil {
		return zero, err
	}
	if status.State == "coordination_active" {
		return activationSummary(status), nil
	}
	// Only a validated, existing restore reaches core.Open. Supported v1 receipt
	// migration preserves predecessor origins before any activation transaction.
	store, err := core.Open(ctx, dataDir)
	if err != nil {
		return zero, unavailable()
	}
	defer store.Close()
	var result ActivationSummary
	err = store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := readRecoveryStatus(ctx, tx)
		if err != nil {
			return err
		}
		if err = checkActivationRequest(ctx, tx, current, in, hash); err != nil {
			return err
		}
		if current.State == "inactive" {
			current.State = "coordination_active"
			current.CredentialID = core.NewID("credential")
			current.ActivatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if _, err = tx.ExecContext(ctx, `INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES(?,?,?,1,0)`, current.CredentialID, core.NewID("principal"), hash); err != nil {
				return unavailable()
			}
			for k, v := range map[string]string{"restore:state": current.State, "restore:provider_state": "held", "restore:activation_credential_id": current.CredentialID, "restore:activated_at": current.ActivatedAt} {
				if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
					return unavailable()
				}
			}
		}
		result = activationSummary(current)
		return nil
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}
func activationSummary(status RecoveryStatus) ActivationSummary {
	return ActivationSummary{InstanceID: status.InstanceID, SourceInstanceID: status.SourceInstanceID, BundleID: status.BundleID, State: status.State, ProvidersHeld: true, CredentialID: status.CredentialID, ActivatedAt: status.ActivatedAt}
}

func activationIntegrity(ctx context.Context, db sqlReader) error {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return invalid()
	}
	count := 0
	for rows.Next() {
		var result string
		if rows.Scan(&result) != nil || result != "ok" {
			rows.Close()
			return invalid()
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != 1 {
		return invalid()
	}
	rows, err = db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return invalid()
	}
	bad := rows.Next()
	err = rows.Err()
	rows.Close()
	if bad || err != nil {
		return invalid()
	}
	return nil
}
