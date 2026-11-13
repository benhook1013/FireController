package server

import (
	"context"
	"database/sql"
	"flag"
	"net/http"
	"time"

	"github.com/benhook1013/FireController/internal/backuphealth"
	"github.com/benhook1013/FireController/internal/core"
)

// WithBackupHealthFiles selects only operator-published redacted projections.
// It never grants the daemon access to offsite configuration or credentials.
func WithBackupHealthFiles(paths []string) Option {
	return func(a *api) { a.backupHealthFiles = append([]string(nil), paths...) }
}

// BackupHealthReader projects retained managed status inside an authorized SQL
// snapshot. Keeping this injected avoids giving the server backup ownership.
type BackupHealthReader func(context.Context, *sql.Tx) (backuphealth.LocalRecord, error)

func WithBackupHealthReader(reader BackupHealthReader) Option {
	return func(a *api) { a.backupHealthReader = reader }
}

// BackupHealthFiles is a bounded repeatable daemon flag. Missing files remain a
// reportable unknown; flag validation never opens an operator file.
type BackupHealthFiles []string

var _ flag.Value = (*BackupHealthFiles)(nil)

func (v *BackupHealthFiles) String() string { return "" }
func (v *BackupHealthFiles) Set(path string) error {
	next := append(append([]string(nil), *v...), path)
	if err := backuphealth.ValidateFiles(next); err != nil {
		return err
	}
	*v = next
	return nil
}

func (a *api) backupHealth(w http.ResponseWriter, r *http.Request) {
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	scope, err := a.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	now := time.Now().UTC()
	result := backuphealth.Snapshot{Version: 1, CheckedAt: now.Format(time.RFC3339Nano), Local: backuphealth.Row{State: "unknown", Reason: "status_unavailable"}, Offsite: []backuphealth.Row{}}
	if err := a.store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		if a.backupHealthReader != nil {
			status, err := a.backupHealthReader(ctx, tx)
			if err == nil {
				result.Local = backuphealth.Local(status, a.store.InstanceID(), now)
			}
		}
		return nil
	}); err != nil {
		core.WriteError(w, err)
		return
	}
	if backuphealth.ValidateFiles(a.backupHealthFiles) != nil {
		result.Offsite = append(result.Offsite, backuphealth.Unknown())
	} else {
		for _, path := range a.backupHealthFiles {
			mirror, err := backuphealth.ReadMirror(path)
			if err != nil {
				result.Offsite = append(result.Offsite, backuphealth.Unknown())
				continue
			}
			result.Offsite = append(result.Offsite, backuphealth.Offsite(mirror, a.store.InstanceID(), now))
		}
	}
	core.WriteJSON(w, http.StatusOK, result)
}
