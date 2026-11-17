package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
)

func TestBackupPublicationHoldKeepsDaemonServingAcrossRestart(t *testing.T) {
	source, instance := backupCommandSource(t)
	destination := filepath.Join(t.TempDir(), "bundles")
	seed, err := core.Open(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	cfg := backup.ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1}
	if _, err := backup.NewManager(seed, source, cfg); err != nil {
		t.Fatal(err)
	}
	status, err := backup.ReadStatus(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	if err := seed.DB().QueryRow(`SELECT body_json FROM managed_backup_state WHERE singleton=1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	needle := `"next_attempt_at":"` + status.NextAttemptAt + `"`
	if strings.Count(body, needle) != 1 {
		t.Fatal("canonical schedule fixture has no unique due timestamp")
	}
	// Move only a validated schedule's due time; publication and failure records
	// below come from a real managed snapshot and failed ownership transaction.
	body = strings.Replace(body, needle, `"next_attempt_at":"`+time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)+`"`, 1)
	if _, err := seed.DB().Exec(`UPDATE managed_backup_state SET body_json=? WHERE singleton=1`, body); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.ReadStatus(t.Context(), source); err != nil {
		t.Fatal("invalid due-time fixture", err)
	}
	manager, err := backup.NewManager(seed, source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// TEMP belongs to the real writer connection and never enters the main
	// database snapshot. Creation remains genuine; only its ledger INSERT fails.
	if err := seed.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TEMP TRIGGER reject_daemon_backup_registration BEFORE INSERT ON main.managed_backup_bundles BEGIN SELECT RAISE(ABORT,'fixture registration rollback'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seedCtx, cancelSeed := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelSeed()
	var reports []error
	if err := manager.Run(seedCtx, func(err error) {
		reports = append(reports, err)
		cancelSeed()
	}); err != nil || len(reports) != 1 {
		t.Fatal("real publication failure fixture did not settle", err, reports)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	held, err := backup.ReadStatus(t.Context(), source)
	if err != nil || held.State != "held" || held.UnregisteredBundle == nil || held.LastSuccess != nil || held.LastAttempt == nil || held.LastAttempt.Reason != "backup_registration_failed" {
		t.Fatal("fixture did not retain a genuine publication hold", held, err)
	}
	if _, err := backup.Inspect(t.Context(), held.UnregisteredBundle.Path); err != nil {
		t.Fatal("fixture lost its actual completed bundle", err)
	}
	tokenBytes, err := os.ReadFile(filepath.Join(source, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		t.Run(fmt.Sprintf("start_%d", restart+1), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			stdout := &output{ready: make(chan struct{})}
			stderr := &output{ready: make(chan struct{})}
			done := make(chan int, 1)
			stopped := false
			t.Cleanup(func() {
				cancel()
				if !stopped {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("owned daemon did not join during cleanup")
					}
				}
			})
			go func() {
				done <- run(ctx, []string{"--data-dir", source, "--listen", "127.0.0.1:0", "--backup-destination", destination, "--backup-interval", "1m", "--backup-keep", "1"}, stdout, stderr)
			}()
			select {
			case <-stdout.ready:
			case code := <-done:
				stopped = true
				t.Fatalf("held destination stopped startup: code=%d diagnostics=%s", code, stderr.String())
			case <-time.After(5 * time.Second):
				t.Fatal("held destination startup deadline")
			}
			// Give an incorrectly completed supervisor time to reach main's
			// worker-exit branch before assessing live HTTP behavior.
			select {
			case code := <-done:
				stopped = true
				t.Fatalf("publication hold stopped HTTP serving: code=%d diagnostics=%s", code, stderr.String())
			case <-time.After(100 * time.Millisecond):
			}
			var started map[string]string
			if err := json.Unmarshal([]byte(stdout.String()), &started); err != nil || started["instance_id"] != instance {
				t.Fatal("held restart changed instance", started, err)
			}
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			for _, path := range []string{"/readyz", "/api/v1/instance"} {
				request, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+started["address"]+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("held destination prevented coordination", path, err)
				}
				responseBody, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr != nil || response.StatusCode != http.StatusOK || path == "/api/v1/instance" && !bytes.Contains(responseBody, []byte(instance)) {
					t.Fatal("held destination lost readiness or authenticated access", path, response.StatusCode, readErr)
				}
			}
			current, err := backup.ReadStatus(t.Context(), source)
			if err != nil || !reflect.DeepEqual(held, current) {
				t.Fatal("held startup rewrote failure or claimed success", current, err)
			}
			entries, err := os.ReadDir(destination)
			if err != nil || len(entries) != 1 || entries[0].Name() != held.UnregisteredBundle.ID {
				t.Fatal("held startup created another backup", entries, err)
			}
			cancel()
			select {
			case code := <-done:
				stopped = true
				if code != 0 {
					t.Fatal("held worker did not join cleanly", code, stderr.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("held worker ignored daemon cancellation")
			}
		})
	}
}
