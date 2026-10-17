package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
)

func TestRestoreActivationManagedScheduleKeepsAllProviderWorkHeld(t *testing.T) {
	f := newActivationCommandFixture(t)
	runBackupResult(t, f.args()...)
	work := activationWorkRows(t, f.dir)
	destination := filepath.Join(t.TempDir(), "restored-generation-backups")
	options := []string{"--backup-destination", destination, "--backup-interval", "1m", "--backup-keep", "1"}
	_, stop := activationStart(t, f, options...)
	status, err := backup.ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "scheduled" || status.LastSuccess != nil || status.NextAttemptAt == "" {
		t.Fatal("held schedule unavailable or inherited work dispatched", status, err)
	}
	stop()
	// A stopped-daemon fixture makes this legitimate schedule due without
	// reducing the supported one-minute interval or simulating backup success.
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var body string
	if err = db.QueryRow(`SELECT body_json FROM managed_backup_state WHERE singleton=1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	needle := `"next_attempt_at":"` + status.NextAttemptAt + `"`
	if strings.Count(body, needle) != 1 {
		t.Fatal("canonical due field missing")
	}
	body = strings.Replace(body, needle, `"next_attempt_at":"`+time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)+`"`, 1)
	_, err = db.Exec(`UPDATE managed_backup_state SET body_json=? WHERE singleton=1`, body)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	base, stop := activationStart(t, f, options...)
	code, result := activationHTTP(t, base, "GET", "/readyz", f.token, f.instance, f.worker, nil)
	if code != 200 {
		t.Fatal("coordination readiness while scheduled", code, result)
	}
	code, result = activationHTTP(t, base, "POST", "/api/v1/projects/"+f.project+"/messages", f.token, f.instance, f.worker, map[string]any{"recipient_worker_id": f.worker, "body": "Coordination on the restored schedule"})
	if code != 201 {
		t.Fatal("coordination unavailable during restored backup", code, result)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err = backup.ReadStatus(t.Context(), f.dir)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "scheduled" && status.LastSuccess != nil && status.Retention != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real restored-generation backup did not complete", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.LastAttempt.State != "complete" || status.LastSuccess.SourceInstanceID != f.instance || status.Retention.State != "complete" {
		t.Fatal("restored backup did not retain true current-generation success", status)
	}
	stop()
	if _, err = backup.Inspect(t.Context(), status.LastSuccess.Path); err != nil {
		t.Fatal("scheduled recovery bundle unverifiable", err)
	}
	if !reflect.DeepEqual(work, activationWorkRows(t, f.dir)) {
		t.Fatal("backup schedule released or rewrote provider work")
	}
	// The same activated copy restarts without flags as coordination-only; its
	// stored schedule cannot start itself, while verified history stays visible.
	_, stop = activationStart(t, f)
	disabled, err := backup.ReadStatus(t.Context(), f.dir)
	if err != nil || disabled.State != "disabled" || disabled.NextAttemptAt != "" || disabled.LastSuccess == nil || disabled.LastSuccess.ID != status.LastSuccess.ID {
		t.Fatal("no-flags restart inherited schedule or lost success", disabled, err)
	}
	stop()
	if !reflect.DeepEqual(work, activationWorkRows(t, f.dir)) {
		t.Fatal("restart released retained provider work")
	}
	for _, name := range []string{"review-workspaces", "hosted-review-captures", "bootstrap-admin.token"} {
		if _, err = os.Lstat(filepath.Join(f.dir, name)); !os.IsNotExist(err) {
			t.Fatal("restored schedule created provider or bootstrap state", name, err)
		}
	}
}

func TestRestoreActivationFutureRunningBackupDoesNotBlockCoordination(t *testing.T) {
	f := newActivationCommandFixture(t)
	runBackupResult(t, f.args()...)
	work := activationWorkRows(t, f.dir)
	destination := filepath.Join(t.TempDir(), "future-running-history")
	_, stop := activationStart(t, f, "--backup-destination", destination, "--backup-interval", "1m", "--backup-keep", "1")
	stop()
	// Retain a valid predecessor attempt whose effective clock was ahead of the
	// new daemon. It must be interrupted without claiming a negative duration.
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Configuration    backup.ScheduleConfig `json:"configuration"`
		Status           backup.Status         `json:"status"`
		SourceInstanceID string                `json:"source_instance_id,omitempty"`
	}
	var body string
	if err = db.QueryRow(`SELECT body_json FROM managed_backup_state WHERE singleton=1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	record.SourceInstanceID = f.predecessor
	record.Status.State = "running"
	record.Status.NextAttemptAt = ""
	record.Status.LastAttempt = &backup.AttemptStatus{StartedAt: started, State: "running", SourceInstanceID: f.predecessor}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE managed_backup_state SET body_json=? WHERE singleton=1`, string(data))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	base, stop := activationStart(t, f)
	code, result := activationHTTP(t, base, "GET", "/readyz", f.token, f.instance, f.worker, nil)
	if code != 200 {
		t.Fatal("future predecessor backup blocked readiness", code, result)
	}
	status, err := backup.ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "disabled" || status.LastAttempt.State != "interrupted" || status.LastAttempt.StartedAt != started || status.LastAttempt.FinishedAt != started || status.LastAttempt.SourceInstanceID != f.predecessor {
		t.Fatal("future predecessor interruption changed facts", status, err)
	}
	stop()
	if !reflect.DeepEqual(work, activationWorkRows(t, f.dir)) {
		t.Fatal("future backup recovery changed provider work")
	}
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("disabled startup resumed predecessor backup", err)
	}
}
