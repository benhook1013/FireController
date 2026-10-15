package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
)

func TestBackupScheduleRejectsInvalidConfigurationBeforeState(t *testing.T) {
	for _, options := range [][]string{
		{"--backup-destination", "DEST"}, {"--backup-interval", "1m"}, {"--backup-keep", "1"},
		{"--backup-timeout", "1m"},
		{"--backup-destination", "DEST", "--backup-interval", "1m"},
		{"--backup-destination", "DEST", "--backup-interval", "59s", "--backup-keep", "1"},
		{"--backup-destination", "DEST", "--backup-interval", "720h1ns", "--backup-keep", "1"},
		{"--backup-destination", "DEST", "--backup-interval", "1m", "--backup-keep", "0"},
		{"--backup-destination", "DEST", "--backup-interval", "1m", "--backup-keep", "1001"},
		{"--backup-destination", "DEST", "--backup-interval", "1m", "--backup-keep", "1", "--backup-timeout", "0"},
		{"--backup-destination", "DEST", "--backup-interval", "1m", "--backup-keep", "1", "--backup-timeout", "24h1ns"},
		{"--backup-destination", "SOURCE", "--backup-interval", "1m", "--backup-keep", "1"},
		{"--backup-destination", "", "--backup-interval", "1m", "--backup-keep", "1"},
		{"--backup-destination", "", "--backup-interval", "0", "--backup-keep", "0"},
	} {
		root := t.TempDir()
		source := filepath.Join(root, "absent-source")
		destination := filepath.Join(root, "absent-destination")
		args := []string{"--data-dir", source, "--listen", "127.0.0.1:0", "--review-catalog", filepath.Join(root, "must-not-load-catalog"), "--github-token-file", filepath.Join(root, "must-not-load-token")}
		for _, option := range options {
			if option == "DEST" {
				option = destination
			}
			if option == "SOURCE" {
				option = source
			}
			args = append(args, option)
		}
		var out, diagnostics bytes.Buffer
		if code := run(t.Context(), args, &out, &diagnostics); code != 2 || out.Len() != 0 || !json.Valid(diagnostics.Bytes()) {
			t.Fatalf("invalid schedule continued: options=%v code=%d diagnostics=%s", options, code, diagnostics.String())
		}
		for _, path := range []string{source, destination} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("invalid schedule created state", err)
			}
		}
	}
}

func TestBackupStatusIsCompactReadOnlyAndCannotBootstrap(t *testing.T) {
	source, instance := backupCommandSource(t)
	live, err := core.Open(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	var before, after int
	if err = live.DB().QueryRow("SELECT count(*) FROM metadata").Scan(&before); err != nil {
		t.Fatal(err)
	}
	var status map[string]json.RawMessage
	if err = json.Unmarshal(runBackupResult(t, "backup-status", "--data-dir", source), &status); err != nil {
		t.Fatal(err)
	}
	if string(status["state"]) != `"unconfigured"` {
		t.Fatal("read invented configured scheduler", string(status["state"]))
	}
	if err = live.DB().QueryRow("SELECT count(*) FROM metadata").Scan(&after); err != nil || before != after || live.InstanceID() != instance {
		t.Fatal("status changed source", err)
	}
	for _, args := range [][]string{
		{"backup-status"}, {"backup-status", "--data-dir", source, "--data-dir", source},
		{"backup-status", "--data-dir", source, "--destination", "private-destination"},
		{"backup-status", "--data-dir", source, "--timeout", "0"},
		{"backup-status", "--data-dir", source, "positional"},
	} {
		var out, diagnostics bytes.Buffer
		if code := run(t.Context(), args, &out, &diagnostics); code != 2 || out.Len() != 0 {
			t.Fatal("invalid status accepted", args, code)
		}
	}
	absent := filepath.Join(t.TempDir(), "missing")
	var out, diagnostics bytes.Buffer
	if code := run(t.Context(), []string{"backup-status", "--data-dir", absent}, &out, &diagnostics); code != 3 || out.Len() != 0 {
		t.Fatal("missing source status accepted", code)
	}
	if _, err = os.Lstat(absent); !os.IsNotExist(err) {
		t.Fatal("status bootstrapped source", err)
	}
}

type backupRunnerFunc func(context.Context, func(error)) error

func (f backupRunnerFunc) Run(ctx context.Context, report func(error)) error { return f(ctx, report) }

func TestBackupWorkerCancellationJoinsAndReportsAttemptFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	cancelSeen := make(chan struct{})
	release := make(chan struct{})
	reported := make(chan error, 1)
	runner := backupRunnerFunc(func(ctx context.Context, report func(error)) error {
		close(started)
		report(core.Fail("backup_unavailable", "The managed backup destination is unavailable.", 503))
		<-ctx.Done()
		close(cancelSeen)
		<-release
		return nil
	})
	done := startBackupWorker(ctx, runner, true, func(err error) { reported <- err })
	<-started
	if err := <-reported; err == nil {
		t.Fatal("per-attempt failure dropped")
	}
	select {
	case <-done:
		t.Fatal("attempt failure stopped worker")
	default:
	}
	cancel()
	<-cancelSeen
	if err := waitBackupWorker(done, time.Millisecond); err == nil {
		t.Fatal("join returned before owned worker finished")
	}
	close(release)
	if err := waitBackupWorker(done, time.Second); err != nil {
		t.Fatal(err)
	}
	if disabled := startBackupWorker(t.Context(), backupRunnerFunc(func(context.Context, func(error)) error { t.Error("disabled worker started"); return nil }), false, nil); disabled != nil {
		t.Fatal("disabled config owns a worker")
	}
	if err := waitBackupWorker(nil, time.Second); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("controlled manager failure")
	done = startBackupWorker(t.Context(), backupRunnerFunc(func(context.Context, func(error)) error { return failure }), true, nil)
	if err := waitBackupWorker(done, time.Second); !errors.Is(err, failure) {
		t.Fatal("worker failure lost", err)
	}
}

func TestBackupScheduleFailureKeepsReadinessAndRestartDisables(t *testing.T) {
	source, instance := backupCommandSource(t)
	if os.Geteuid() == 0 {
		t.Skip("destination permission refusal requires an unprivileged Unix owner")
	}
	destination := filepath.Join(t.TempDir(), "bundles")
	if err := os.Mkdir(destination, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(destination, 0700) })
	seed, err := core.Open(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if err = backup.EnsureManagedSchema(t.Context(), seed); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	if _, err = backup.NewManager(seed, source, backup.ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1}); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	if _, err = backup.ReadStatus(t.Context(), source); err != nil {
		seed.Close()
		t.Fatal("new manager produced invalid stored status", err)
	}
	// Model an already-due retained schedule without a real minute delay. The
	// constructor/read decoder still validate the complete production record.
	err = seed.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE managed_backup_state SET body_json=json_set(body_json,'$.status.next_attempt_at',?)", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano))
		return err
	})
	if err == nil {
		_, err = backup.ReadStatus(t.Context(), source)
	}
	_ = seed.Close()
	if err != nil {
		t.Fatal("overdue schedule fixture invalid", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := &output{ready: make(chan struct{})}
	stderr := &output{ready: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--data-dir", source, "--listen", "127.0.0.1:0", "--backup-destination", destination, "--backup-interval", "1m", "--backup-keep", "1"}, stdout, stderr)
	}()
	select {
	case <-stdout.ready:
	case code := <-done:
		t.Fatalf("schedule prevented serving code=%d diagnostics=%s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("startup deadline")
	}
	select {
	case <-stderr.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("missing managed destination failure")
	}
	var started map[string]string
	if err := json.Unmarshal([]byte(stdout.String()), &started); err != nil {
		t.Fatal(err)
	}
	if started["instance_id"] != instance {
		t.Fatal("schedule changed identity")
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + started["address"] + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	client.CloseIdleConnections()
	if response.StatusCode != http.StatusOK {
		t.Fatal("attempt failure stopped readiness")
	}
	if !json.Valid([]byte(stderr.String())) || strings.Contains(stderr.String(), destination) || strings.Contains(stderr.String(), source) {
		t.Fatal("attempt diagnostic reflected paths or invalid JSON")
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal("schedule cancellation failed", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("owned worker did not settle")
	}
	status := runBackupResult(t, "backup-status", "--data-dir", source)
	var facts struct {
		State       string `json:"state"`
		LastAttempt *struct {
			State string `json:"state"`
		} `json:"last_attempt"`
	}
	if err = json.Unmarshal(status, &facts); err != nil || facts.LastAttempt == nil || facts.LastAttempt.State != "failed" {
		t.Fatal("status lost failed attempt", err)
	}
	// Explicitly omit scheduling on the next ordinary restart. The constructor
	// must persist disabled without erasing the previous failure or identity.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	stdout2 := &output{ready: make(chan struct{})}
	stderr2 := &output{ready: make(chan struct{})}
	done2 := make(chan int, 1)
	go func() {
		done2 <- run(ctx2, []string{"--data-dir", source, "--listen", "127.0.0.1:0"}, stdout2, stderr2)
	}()
	select {
	case <-stdout2.ready:
	case code := <-done2:
		t.Fatalf("restart failed code=%d diagnostics=%s", code, stderr2.String())
	case <-time.After(10 * time.Second):
		t.Fatal("restart deadline")
	}
	if err = json.Unmarshal([]byte(stdout2.String()), &started); err != nil || started["instance_id"] != instance {
		t.Fatal("restart changed identity", err)
	}
	cancel2()
	select {
	case code := <-done2:
		if code != 0 {
			t.Fatal("disabled restart shutdown failed", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("restart shutdown deadline")
	}
	if err = json.Unmarshal(runBackupResult(t, "backup-status", "--data-dir", source), &facts); err != nil || facts.State != "disabled" || facts.LastAttempt == nil || facts.LastAttempt.State != "failed" {
		t.Fatal("restart did not truthfully disable schedule", err)
	}
}

func TestBackupStatusRefusesIncompleteAndInactiveRestores(t *testing.T) {
	source, _ := backupCommandSource(t)
	marker := filepath.Join(source, ".restore-in-progress")
	if err := os.WriteFile(marker, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if code := run(t.Context(), []string{"backup-status", "--data-dir", source}, &out, &diagnostics); code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"restore_inactive"`) {
		t.Fatal("status ignored incomplete restore fence", code, diagnostics.String())
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	var created backup.Summary
	if err := json.Unmarshal(runBackupResult(t, "backup", "--data-dir", source, "--destination", filepath.Join(t.TempDir(), "bundles")), &created); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "inactive")
	runBackupResult(t, "restore", "--bundle", created.Path, "--data-dir", restored)
	out.Reset()
	diagnostics.Reset()
	if code := run(t.Context(), []string{"backup-status", "--data-dir", restored}, &out, &diagnostics); code != 3 || out.Len() != 0 || !json.Valid(diagnostics.Bytes()) {
		t.Fatal("status ignored inactive restore fence", code, diagnostics.String())
	}
	if _, err := os.Lstat(filepath.Join(restored, "bootstrap-admin.token")); !os.IsNotExist(err) {
		t.Fatal("status created restored credential", err)
	}
}
