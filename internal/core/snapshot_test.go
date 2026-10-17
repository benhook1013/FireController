package core

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestSnapshotSelfContainedDuringTransactionalWrites(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	store, err := Open(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		for _, statement := range []string{
			`CREATE TABLE snapshot_left(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE snapshot_right(id INTEGER PRIMARY KEY REFERENCES snapshot_left(id))`,
			`CREATE TABLE snapshot_payload(data BLOB NOT NULL)`,
			`INSERT INTO snapshot_payload(data) VALUES(zeroblob(8388608))`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	instance := store.InstanceID()
	var committed atomic.Int64
	stop := make(chan struct{})
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for n := int64(1); ; n++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			err := store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_left(id) VALUES(?)`, n); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `INSERT INTO snapshot_right(id) VALUES(?)`, n)
				return err
			})
			if err != nil {
				done <- err
				return
			}
			committed.Store(n)
			if n == 1 {
				close(started)
			}
		}
	}()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			close(stop)
			<-done
		}
	})
	select {
	case <-started:
	case err = <-done:
		stopped = true
		t.Fatal("fixture writer did not start", err)
	case <-t.Context().Done():
		t.Fatal("fixture was canceled before the writer started")
	}
	staging := filepath.Join(t.TempDir(), "staging")
	if err = os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(staging, "snapshot ' quoted ?.sqlite")
	if err = store.Snapshot(t.Context(), destination); err != nil {
		t.Fatal(err)
	}
	close(stop)
	err = <-done
	stopped = true
	if err != nil || store.InstanceID() != instance {
		t.Fatal("snapshot interfered with source writer or identity", err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	// No source DB, WAL, shared-memory file or source connection may supply
	// missing snapshot contents after this point.
	if err = os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	u := databaseURL(destination)
	u.RawQuery = url.Values{"mode": {"ro"}, "immutable": {"1"}, "_pragma": {"foreign_keys(1)"}}.Encode()
	snapshot, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var integrity, gotInstance string
	var left, right, payload int64
	if err = snapshot.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("snapshot integrity", integrity, err)
	}
	if err = snapshot.QueryRow(`SELECT (SELECT count(*) FROM snapshot_left),(SELECT count(*) FROM snapshot_right),(SELECT length(data) FROM snapshot_payload)`).Scan(&left, &right, &payload); err != nil || left < 1 || left != right || left > committed.Load() || payload != 8388608 {
		t.Fatal("snapshot is not a complete transaction prefix", left, right, payload, err)
	}
	rows, err := snapshot.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() || rows.Err() != nil {
		rows.Close()
		t.Fatal("snapshot foreign-key inconsistency")
	}
	rows.Close()
	if err = snapshot.QueryRow(`SELECT value FROM metadata WHERE key='instance_id'`).Scan(&gotInstance); err != nil || gotInstance != instance {
		t.Fatal("snapshot changed instance identity", gotInstance, err)
	}
}

func TestSnapshotRefusesExistingUnsafeAndCanceledDestinations(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent := filepath.Join(t.TempDir(), "staging")
	if err = os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(parent, "existing.sqlite")
	if err = os.WriteFile(existing, []byte("retain exact bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	err = store.Snapshot(t.Context(), existing)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "backup_exists" {
		t.Fatal("existing destination accepted", err)
	}
	if body, readErr := os.ReadFile(existing); readErr != nil || string(body) != "retain exact bytes" {
		t.Fatal("existing destination changed", readErr)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := filepath.Join(parent, "canceled.sqlite")
	if err = store.Snapshot(ctx, canceled); err == nil {
		t.Fatal("canceled snapshot succeeded")
	}
	if _, err = os.Lstat(canceled); !os.IsNotExist(err) {
		t.Fatal("canceled snapshot left a destination", err)
	}
	if err = store.Snapshot(t.Context(), "relative.sqlite"); err == nil {
		t.Fatal("relative destination accepted")
	}
	if runtime.GOOS != "windows" {
		public := filepath.Join(parent, "public")
		if err = os.Mkdir(public, 0755); err != nil {
			t.Fatal(err)
		}
		if err = store.Snapshot(t.Context(), filepath.Join(public, "unsafe.sqlite")); err == nil {
			t.Fatal("public snapshot directory accepted")
		}
		link := filepath.Join(parent, "link")
		if err = os.Symlink(parent, link); err != nil {
			t.Fatal(err)
		}
		if err = store.Snapshot(t.Context(), filepath.Join(link, "unsafe.sqlite")); err == nil {
			t.Fatal("symlink destination directory accepted")
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(parent, "failed.sqlite")
	if err = store.Snapshot(t.Context(), failed); !errors.As(err, &failure) || failure.Code != "backup_unavailable" {
		t.Fatal("closed source accepted", err)
	}
	if _, err = os.Lstat(failed); !os.IsNotExist(err) {
		t.Fatal("failed snapshot retained its partial destination", err)
	}
}
