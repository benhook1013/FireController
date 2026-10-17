package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const (
	maxManagedStatusBytes = 16 << 10
	maxManagedRecordBytes = 32 << 10
	// Manual bundle/inspect summaries retain their existing newline-inclusive cap.
	maxManualSummaryBytes = 4096
)

type AttemptStatus struct {
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
	State            string `json:"state"`
	Reason           string `json:"reason"`
	SourceInstanceID string `json:"source_instance_id,omitempty"`
}
type RetentionStatus struct {
	AttemptedAt string `json:"attempted_at"`
	State       string `json:"state"`
	Reason      string `json:"reason"`
}

// Status is stored operator history. A past success is not a present-byte
// verification; ReadStatus never inventories or hashes the destination.
type Status struct {
	State              string           `json:"state"`
	Destination        string           `json:"destination"`
	Interval           string           `json:"interval"`
	Timeout            string           `json:"timeout"`
	Keep               int              `json:"keep"`
	NextAttemptAt      string           `json:"next_attempt_at"`
	LastAttempt        *AttemptStatus   `json:"last_attempt"`
	LastSuccess        *Summary         `json:"last_success"`
	UnregisteredBundle *Summary         `json:"unregistered_bundle,omitempty"`
	Retention          *RetentionStatus `json:"retention"`
}

// Configuration must remain reportable even when a prior maximal manual
// summary and failure history are retained. These are encoded output budgets,
// independent of a particular filesystem or SQLite VFS pathname limit.
func scheduleStatusFits(cfg ScheduleConfig) bool {
	const latest = "9999-12-31T23:59:59.999999999Z"
	status := Status{State: "scheduled", Destination: cfg.Destination, Interval: cfg.Interval.String(), Timeout: cfg.Timeout.String(), Keep: cfg.Keep, NextAttemptAt: latest,
		LastAttempt: &AttemptStatus{StartedAt: latest, FinishedAt: latest, State: "interrupted", Reason: "backup_retention_interrupted", SourceInstanceID: "instance_00000000000000000000000000000000"},
		Retention:   &RetentionStatus{AttemptedAt: latest, State: "failed", Reason: "backup_retention_interrupted"}}
	public, err := json.Marshal(status)
	// Allow two maximal summaries: replacing LastSuccess's null and adding
	// the optional unregistered publication, including its JSON field name.
	const summaryGrowth = 2*(maxManualSummaryBytes-1) - len("null") + len(`,"unregistered_bundle":`)
	if err != nil || len(public)+summaryGrowth+1 > maxManagedStatusBytes {
		return false
	}
	private, err := json.Marshal(managedRecord{Configuration: cfg, Status: status, SourceInstanceID: "instance_00000000000000000000000000000000"})
	return err == nil && len(private)+summaryGrowth <= maxManagedRecordBytes
}

type managedRecord struct {
	Configuration    ScheduleConfig `json:"configuration"`
	Status           Status         `json:"status"`
	SourceInstanceID string         `json:"source_instance_id,omitempty"`
}

func loadManaged(ctx context.Context, db sqlReader) (*managedRecord, error) {
	rows, err := db.QueryContext(ctx, `SELECT singleton,CASE WHEN length(CAST(body_json AS BLOB))<=? THEN body_json ELSE NULL END FROM managed_backup_state LIMIT 2`, maxManagedRecordBytes)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	var result *managedRecord
	for rows.Next() {
		var id int
		var body sql.NullString
		if result != nil || rows.Scan(&id, &body) != nil || id != 1 || !body.Valid {
			return nil, invalid()
		}
		var r managedRecord
		if strictjson.Validate([]byte(body.String)) != nil || json.Unmarshal([]byte(body.String), &r) != nil {
			return nil, invalid()
		}
		encoded, e := encodeManaged(r)
		if e != nil || string(encoded) != body.String {
			return nil, invalid()
		}
		result = &r
	}
	if rows.Err() != nil {
		return nil, unavailable()
	}
	return result, nil
}
func encodeManaged(r managedRecord) ([]byte, error) {
	if r.SourceInstanceID != "" && !instanceID.MatchString(r.SourceInstanceID) {
		return nil, invalid()
	}
	c, err := normalizeScheduleConfig("", r.Configuration, false)
	if err != nil || c != r.Configuration {
		return nil, invalid()
	}
	s := r.Status
	if s.Destination != c.Destination || s.Keep != c.Keep {
		return nil, invalid()
	}
	if c.Destination == "" {
		if s.State != "disabled" || s.Interval != "" || s.Timeout != "" || s.NextAttemptAt != "" {
			return nil, invalid()
		}
	} else if (s.State != "scheduled" && s.State != "running" && s.State != "held") || s.Interval != c.Interval.String() || s.Timeout != c.Timeout.String() {
		return nil, invalid()
	}
	if s.State == "held" && (s.UnregisteredBundle == nil || s.NextAttemptAt != "" || filepath.Dir(s.UnregisteredBundle.Path) != c.Destination || s.UnregisteredBundle.SourceInstanceID != r.SourceInstanceID) {
		return nil, invalid()
	}
	if s.State == "running" && s.LastAttempt == nil || s.State == "scheduled" && s.NextAttemptAt == "" || s.State == "running" && s.NextAttemptAt != "" {
		return nil, invalid()
	}
	if s.NextAttemptAt != "" {
		if _, ok := canonicalTime(s.NextAttemptAt); !ok {
			return nil, invalid()
		}
	}
	if a := s.LastAttempt; a != nil {
		if a.SourceInstanceID != "" && !instanceID.MatchString(a.SourceInstanceID) {
			return nil, invalid()
		}
		start, ok := canonicalTime(a.StartedAt)
		if !ok {
			return nil, invalid()
		}
		if a.State == "running" {
			if a.FinishedAt != "" || a.Reason != "" || s.State != "running" {
				return nil, invalid()
			}
		} else {
			finish, ok := canonicalTime(a.FinishedAt)
			if !ok || finish.Before(start) {
				return nil, invalid()
			}
			switch a.State {
			case "complete":
				if a.Reason != "" {
					return nil, invalid()
				}
			case "failed", "interrupted":
				if !managedReason(a.Reason) {
					return nil, invalid()
				}
			default:
				return nil, invalid()
			}
		}
	}
	for _, last := range []*Summary{s.LastSuccess, s.UnregisteredBundle} {
		if last == nil {
			continue
		}
		start, ok := canonicalTime(last.SnapshotStartedAt)
		end, eok := canonicalTime(last.SnapshotFinishedAt)
		if !backupID.MatchString(last.ID) || !instanceID.MatchString(last.SourceInstanceID) || last.State != "complete" || !filepath.IsAbs(last.Path) || filepath.Base(last.Path) != last.ID || !ok || !eok || end.Before(start) || last.EventSequence < 0 || last.DatabaseBytes <= 0 || last.ArtifactCount < 0 || last.ArtifactCount > maxArtifacts || last.ArtifactBytes < 0 {
			return nil, invalid()
		}
	}
	if v := s.Retention; v != nil {
		if _, ok := canonicalTime(v.AttemptedAt); !ok {
			return nil, invalid()
		}
		if v.State == "complete" {
			if v.Reason != "" {
				return nil, invalid()
			}
		} else if v.State != "failed" || !managedReason(v.Reason) {
			return nil, invalid()
		}
	}
	public, err := json.Marshal(s)
	if err != nil || len(public)+1 > maxManagedStatusBytes {
		return nil, invalid()
	}
	body, err := json.Marshal(r)
	if err != nil || len(body) > maxManagedRecordBytes {
		return nil, invalid()
	}
	return body, nil
}
func storeManaged(ctx context.Context, tx *sql.Tx, r managedRecord) error {
	body, err := encodeManaged(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO managed_backup_state(singleton,body_json) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET body_json=excluded.body_json`, string(body))
	return err
}

// ReadStatus reads one retained singleton in a read-only live SQLite snapshot.
// Older valid installations remain unconfigured; no schema is installed here.
func ReadStatus(ctx context.Context, dataDir string) (Status, error) {
	if checkContext(ctx) != nil {
		return Status{}, unavailable()
	}
	if err := CheckDataDirectory(dataDir); err != nil {
		return Status{}, err
	}
	if privateDirectory(dataDir, false) != nil {
		return Status{}, invalid()
	}
	db, err := openDatabase(ctx, filepath.Join(dataDir, "firecontroller.sqlite"), false)
	if err != nil {
		return Status{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Status{}, unavailable()
	}
	defer tx.Rollback()
	versions, err := validateSchema(ctx, tx, false)
	if err != nil {
		return Status{}, err
	}
	var identity string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='instance_id'`).Scan(&identity); err != nil || !instanceID.MatchString(identity) {
		return Status{}, invalid()
	}
	if versions["managed_backups"] == 0 {
		return Status{State: "unconfigured"}, nil
	}
	r, err := loadManaged(ctx, tx)
	if err != nil {
		return Status{}, err
	}
	if r == nil {
		return Status{State: "unconfigured"}, nil
	}
	return r.Status, nil
}
func managedReason(reason string) bool {
	switch reason {
	case "backup_invalid", "backup_unavailable", "backup_interrupted", "backup_retention_failed", "backup_retention_interrupted", "backup_registration_failed":
		return true
	}
	return false
}
