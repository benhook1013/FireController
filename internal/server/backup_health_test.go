package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backuphealth"
	"github.com/benhook1013/FireController/internal/server"
)

func TestBackupHealthPrivateProjectScopeAndRedactedStoredFacts(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	other := f.project(t, "owner/two")
	token := "synthetic-backup-health-reader"
	f.grant(t, project, token, false)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mirror.json")
	now := time.Now().UTC()
	stamp := now.Add(-time.Minute).Format(time.RFC3339Nano)
	m := backuphealth.Mirror{Version: 1, InstanceID: f.store.InstanceID(), ObservedAt: stamp, Label: "Cloud copy", MaxAgeSeconds: 10800, LastSnapshotAt: now.Add(-time.Hour).Format(time.RFC3339Nano), AttemptState: "failed", LastAttemptAt: stamp, AttemptReason: "offsite_transport_failed"}
	if err := backuphealth.WriteMirror(path, m); err != nil {
		t.Fatal(err)
	}
	var reads int
	f.handler = server.New(f.store, server.WithBackupHealthFiles([]string{path}), server.WithBackupHealthReader(func(context.Context, *sql.Tx) (backuphealth.LocalRecord, error) {
		reads++
		return backuphealth.LocalRecord{}, errors.New("PRIVATE_PATH_ACCOUNT_CREDENTIAL")
	}))
	endpoint := "/api/v1/projects/" + project + "/backup-health"
	beforeEvents, beforeReceipts := count(t, f.store, "events"), count(t, f.store, "receipts")
	for _, denied := range []struct {
		path, token string
		status      int
	}{{endpoint, "", 401}, {"/api/v1/projects/" + other + "/backup-health", token, 403}} {
		w := request(f.handler, "GET", denied.path, denied.token, "", "", "")
		require(t, w, denied.status)
	}
	if reads != 0 {
		t.Fatal("unauthorized request read backup facts")
	}
	w := request(f.handler, "GET", endpoint, token, "", "", "")
	require(t, w, 200)
	var result backuphealth.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != 1 || result.CheckedAt == "" || result.Local.State != "unknown" || len(result.Offsite) != 1 || result.Offsite[0].State != "failed" || result.Offsite[0].LastSuccessAt != m.LastSnapshotAt || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Body.String(), w.Header())
	}
	for _, secret := range []string{"PRIVATE_PATH_ACCOUNT_CREDENTIAL", path, dir, "archive_sha256", "instance_id", "destination", "offsite_transport_failed"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private operator state leaked", secret)
		}
	}
	id := strings.Repeat("b", 32)
	cookie := browserLogin(t, f, project, token, id, "localhost")
	require(t, browserRequest(f, "GET", endpoint, "", id, "", "localhost", cookie), 200)
	require(t, browserRequest(f, "GET", "/api/v1/projects/"+other+"/backup-health", "", id, "", "localhost", cookie), 403)
	require(t, request(f.handler, "GET", endpoint+"?path=PRIVATE", token, "", "", ""), 400)
	require(t, request(f.handler, "POST", endpoint, token, "", "", ""), http.StatusNotFound)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_read=0 WHERE project_id=?`, project); err != nil {
		t.Fatal(err)
	}
	require(t, browserRequest(f, "GET", endpoint, "", id, "", "localhost", cookie), 403)
	if count(t, f.store, "events") != beforeEvents || count(t, f.store, "receipts") != beforeReceipts {
		t.Fatal("health reads emitted domain work")
	}
}

func TestBackupHealthMissingStaleWrongInstanceAndNoReporterAreDistinct(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mirror.json")
	endpoint := "/api/v1/projects/" + project + "/backup-health"
	for _, mode := range []string{"not-configured", "missing", "stale", "wrong-instance"} {
		t.Run(mode, func(t *testing.T) {
			paths := []string{path}
			if mode == "not-configured" {
				paths = nil
			}
			if mode == "stale" || mode == "wrong-instance" {
				now := time.Now().UTC()
				m := backuphealth.Mirror{Version: 1, InstanceID: f.store.InstanceID(), ObservedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Label: "Cloud", MaxAgeSeconds: 10800, LastSnapshotAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano)}
				if mode == "wrong-instance" {
					m.InstanceID = "instance_" + strings.Repeat("f", 32)
					m.ObservedAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
				}
				if err := backuphealth.WriteMirror(path, m); err != nil {
					t.Fatal(err)
				}
			}
			f.handler = server.New(f.store, server.WithBackupHealthFiles(paths), server.WithBackupHealthReader(func(context.Context, *sql.Tx) (backuphealth.LocalRecord, error) {
				return backuphealth.LocalRecord{}, nil
			}))
			w := request(f.handler, "GET", endpoint, f.admin, "", "", "")
			require(t, w, 200)
			var r backuphealth.Snapshot
			json.Unmarshal(w.Body.Bytes(), &r)
			if r.Local.State != "unconfigured" {
				t.Fatal(r)
			}
			if mode == "not-configured" {
				if r.Offsite == nil || len(r.Offsite) != 0 {
					t.Fatal(r)
				}
			} else if len(r.Offsite) != 1 || r.Offsite[0].State != "unknown" {
				t.Fatal(r)
			}
			if mode == "wrong-instance" && r.Offsite[0].LastSuccessAt != "" {
				t.Fatal("other instance facts escaped", r)
			}
		})
	}
}
