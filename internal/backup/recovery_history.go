package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const maxRecoveryGenerations = 1000
const maxRecoveryHistoryBytes = 4096
const recoveryHistoryDDL = `CREATE TABLE restore_history(instance_id TEXT PRIMARY KEY,body_json TEXT NOT NULL)`

func readRestoreFields(ctx context.Context, db sqlReader) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key,value FROM metadata WHERE key GLOB 'restore:*' LIMIT 10`)
	if err != nil {
		return nil, unavailable()
	}
	fields := map[string]string{}
	for rows.Next() {
		var key, value string
		if rows.Scan(&key, &value) != nil {
			rows.Close()
			return nil, unavailable()
		}
		fields[key] = value
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, unavailable()
	}
	return fields, nil
}
func recoveryHistoryPresent(ctx context.Context, db sqlReader) (bool, error) {
	var version string
	err := db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:recovery_history'`).Scan(&version)
	var table int
	if db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='restore_history'`).Scan(&table) != nil {
		return false, invalid()
	}
	if errors.Is(err, sql.ErrNoRows) && table == 0 {
		return false, nil
	}
	if err != nil || version != "1" || table != 1 {
		return false, invalid()
	}
	if err := validateRecoveryHistoryShape(ctx, db); err != nil {
		return false, err
	}
	return true, nil
}

func recoveryHistoryColumn(col, kind string, notnull, pk, hidden int, defaultValue any) bool {
	return kind == "TEXT" && defaultValue == nil && hidden == 0 &&
		(col == "instance_id" && pk == 1 && notnull == 0 || col == "body_json" && pk == 0 && notnull == 1)
}

// The recovery status/startup boundary validates this optional shape directly,
// without depending on the full backup schema inventory or recursing into it.
func validateRecoveryHistoryShape(ctx context.Context, db sqlReader) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_xinfo("restore_history")`)
	if err != nil {
		return invalid()
	}
	seen := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk, hidden int
		var col, kind string
		var defaultValue any
		if rows.Scan(&cid, &col, &kind, &notnull, &defaultValue, &pk, &hidden) != nil || seen[col] || !recoveryHistoryColumn(col, kind, notnull, pk, hidden, defaultValue) {
			rows.Close()
			return invalid()
		}
		seen[col] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(seen) != 2 {
		return invalid()
	}
	return validateForeignKeys(ctx, db, "restore_history")
}

// validateRecoveryHistory proves one connected predecessor chain. It never
// refreshes timestamps, adopts unrelated rows or rewrites retained JSON bytes.
func validateRecoveryHistory(ctx context.Context, db sqlReader, current RecoveryStatus, sequence int64) error {
	present, err := recoveryHistoryPresent(ctx, db)
	if err != nil || !present {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT instance_id,CASE WHEN length(CAST(body_json AS BLOB))<=4096 THEN body_json ELSE NULL END FROM restore_history ORDER BY instance_id LIMIT 1001`)
	if err != nil {
		return invalid()
	}
	bodies := map[string]string{}
	for rows.Next() {
		var id string
		var body sql.NullString
		if rows.Scan(&id, &body) != nil || !instanceID.MatchString(id) || id == current.InstanceID || !body.Valid || len(bodies) >= maxRecoveryGenerations {
			rows.Close()
			return invalid()
		}
		if _, exists := bodies[id]; exists {
			rows.Close()
			return invalid()
		}
		bodies[id] = body.String
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return invalid()
	}
	if len(bodies) == 0 {
		return nil
	}
	if current.State == "ordinary" {
		return invalid()
	}
	visited := map[string]bool{current.InstanceID: true}
	credentials := map[string]bool{}
	if current.CredentialID != "" {
		credentials[current.CredentialID] = true
	}
	next := current.SourceInstanceID
	count := 0
	for {
		if visited[next] {
			return invalid()
		}
		body, ok := bodies[next]
		if !ok {
			break
		}
		visited[next] = true
		count++
		var fields map[string]string
		if json.Unmarshal([]byte(body), &fields) != nil {
			return invalid()
		}
		canonical, err := json.Marshal(fields)
		if err != nil || string(canonical) != body {
			return invalid()
		}
		prior, priorSequence, err := recoveryStatusFields(ctx, db, next, fields, sequence)
		if err != nil || prior.State != "coordination_active" || credentials[prior.CredentialID] {
			return invalid()
		}
		var revoked int
		if db.QueryRowContext(ctx, `SELECT revoked FROM credentials WHERE id=?`, prior.CredentialID).Scan(&revoked) != nil || revoked != 1 {
			return invalid()
		}
		credentials[prior.CredentialID] = true
		next = prior.SourceInstanceID
		sequence = priorSequence
	}
	if count != len(bodies) {
		return invalid()
	}
	return nil
}

// Only Restore calls this inside its copied destination transaction, before
// replacing current metadata and revoking every credential in the new instance.
func archiveRecoveryGeneration(ctx context.Context, tx *sql.Tx, current RecoveryStatus) error {
	present, err := recoveryHistoryPresent(ctx, tx)
	if err != nil {
		return err
	}
	if present {
		var count int
		if tx.QueryRowContext(ctx, `SELECT count(*) FROM restore_history`).Scan(&count) != nil || count >= maxRecoveryGenerations {
			return invalid()
		}
	} else {
		if _, err = tx.ExecContext(ctx, recoveryHistoryDDL); err != nil {
			return unavailable()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('schema:recovery_history','1')`); err != nil {
			return unavailable()
		}
	}
	fields, err := readRestoreFields(ctx, tx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(fields)
	if err != nil || len(body) > maxRecoveryHistoryBytes || len(fields) != 9 {
		return invalid()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO restore_history(instance_id,body_json) VALUES(?,?)`, current.InstanceID, string(body)); err != nil {
		return unavailable()
	}
	return nil
}
