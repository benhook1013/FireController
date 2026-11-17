package offsite

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backuphealth"
)

func TestOffsiteHealthPublishesConfiguredAttemptTimeoutWithoutChangingStatus(t *testing.T) {
	f := newFixture(t)
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.MkdirAll(f.c.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	state := Status{Version: 1, Binding: binding(f.c), Owned: []Receipt{}, LastAttempt: &Attempt{State: "running", StartedAt: started}}
	statePath := filepath.Join(f.c.StateDir, "offsite.json")
	if err := writeJSON(statePath, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "health.json")
	for _, tc := range []struct {
		timeout string
		seconds int64
	}{{"15m", 900}, {"1500ms", 2}, {"24h", 86400}} {
		f.c.Timeout = tc.timeout
		if err := writeJSON(config, f.c); err != nil {
			t.Fatal(err)
		}
		ordinary, err := ReadStatus(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		report, err := ReadStatusAndWriteHealth(t.Context(), config, output, "Cloud", 3*time.Hour)
		if err != nil || !reflect.DeepEqual(ordinary, report) {
			t.Fatal("health publication changed ordinary raw status", ordinary, report, err)
		}
		mirror, err := backuphealth.ReadMirror(output)
		if err != nil || mirror.AttemptTimeoutSeconds != tc.seconds || mirror.AttemptState != "running" || mirror.LastAttemptAt != started {
			t.Fatal("configured timeout or original attempt not projected", mirror, err)
		}
		if tc.timeout == "15m" && backuphealth.Offsite(mirror, f.c.InstanceID, time.Now().UTC()).State != "running" {
			t.Fatal("ten-minute running attempt falsely stale", mirror)
		}
		body, _ := json.Marshal(report)
		if bytes.Contains(body, []byte("attempt_timeout_seconds")) {
			t.Fatal("mirror-only field changed raw status output")
		}
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("status publication rewrote transfer ledger", err)
	}
}

func TestOffsiteHealthPreservesLedgerAcrossAttemptClockRollback(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	transport := memory()
	if _, err := push(t.Context(), f.c, transport); err != nil {
		t.Fatal(err)
	}
	success := state(t, f.c)
	f.bundle(t)
	// Credential refusals use this same normalized transport failure code;
	// the local fixture exercises it without contacting a provider.
	transport.uncertainReceipt = true
	_, err := push(t.Context(), f.c, transport)
	wantCode(t, err, "offsite_transport_failed")
	failed := state(t, f.c)
	if failed.LastAttempt.Reason != "offsite_transport_failed" || failed.Pending == nil || failed.LastSuccess == nil {
		t.Fatal("fixture did not retain ordinary failure recovery state", failed)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	if err := writeJSON(config, f.c); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "health.json")
	ledger := filepath.Join(f.c.StateDir, "offsite.json")
	finished := time.Now().UTC().Add(-2 * time.Minute)
	started := finished.Add(time.Minute)
	for _, tc := range []struct {
		name, health string
		state        Status
	}{{"success", "healthy", success}, {"failed", "failed", failed}} {
		t.Run(tc.name, func(t *testing.T) {
			tc.state.LastAttempt.StartedAt = started.Format(time.RFC3339Nano)
			tc.state.LastAttempt.FinishedAt = finished.Format(time.RFC3339Nano)
			if err := writeJSON(ledger, tc.state); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := ReadStatus(t.Context(), config)
			if err != nil || !reflect.DeepEqual(raw.LastAttempt, tc.state.LastAttempt) {
				t.Fatal("raw retained status refused valid wall-clock correction", raw, err)
			}
			report, err := ReadStatusAndWriteHealth(t.Context(), config, output, "Cloud", 3*time.Hour)
			if err != nil || !reflect.DeepEqual(report, raw) {
				t.Fatal("health disagreed with the original retained status", report, raw, err)
			}
			mirror, err := backuphealth.ReadMirror(output)
			if err != nil || mirror.AttemptState != raw.LastAttempt.State || mirror.LastAttemptAt != raw.LastAttempt.FinishedAt || mirror.LastSnapshotAt != raw.LastSuccess.SnapshotFinishedAt {
				t.Fatal("health lost original completion/snapshot facts", mirror, err)
			}
			if tc.health == "failed" && (report.LastAttempt.Reason != "offsite_transport_failed" || mirror.AttemptReason != "last_attempt_failed") || tc.health == "healthy" && mirror.AttemptReason != "" {
				t.Fatal("failure reason was hidden or falsely attached to success", report, mirror)
			}
			if got := backuphealth.Offsite(mirror, f.c.InstanceID, time.Now().UTC()); got.State != tc.health {
				t.Fatal("clock correction changed reported outcome", got)
			}
			after, err := os.ReadFile(ledger)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("health rewrote the original ledger/timestamps", err)
			}
		})
	}
	// The successful projection must not make malformed or incoherent attempt
	// records acceptable, nor refresh the report when validation fails.
	previous, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Attempt)
	}{
		{"missing-start", func(a *Attempt) { a.StartedAt = "" }},
		{"missing-finish", func(a *Attempt) { a.FinishedAt = "" }},
		{"malformed-start", func(a *Attempt) { a.StartedAt = "not a timestamp" }},
		{"malformed-finish", func(a *Attempt) { a.FinishedAt = "not a timestamp" }},
		{"noncanonical-start", func(a *Attempt) { a.StartedAt = started.Format("2006-01-02T15:04:05.999999999-07:00") }},
		{"noncanonical-finish", func(a *Attempt) { a.FinishedAt = finished.Format("2006-01-02T15:04:05.999999999-07:00") }},
		{"zero-finish", func(a *Attempt) { a.FinishedAt = time.Time{}.Format(time.RFC3339Nano) }},
		{"finished-running", func(a *Attempt) { a.State = "running" }},
		{"successful-with-failure-reason", func(a *Attempt) { a.Reason = "offsite_transport_failed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := *success.LastAttempt
			tc.change(&attempt)
			invalid := success
			invalid.LastAttempt = &attempt
			if err := writeJSON(ledger, invalid); err != nil {
				t.Fatal(err)
			}
			_, err := ReadStatusAndWriteHealth(t.Context(), config, output, "Cloud", 3*time.Hour)
			wantCode(t, err, "offsite_state_invalid")
			after, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(previous, after) {
				t.Fatal("invalid attempt refreshed the health report", err)
			}
		})
	}
}

func TestOffsiteHealthExportsRetainedFactsWithoutProviderOrBundleAccess(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	tr := memory()
	if _, err := push(t.Context(), f.c, tr); err != nil {
		t.Fatal(err)
	}
	// A later unchanged run must not invent a fresh remote verification time.
	if _, err := push(t.Context(), f.c, tr); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	if err := writeJSON(config, f.c); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "health.json")
	moved := f.c.SourceRoot + "-unavailable"
	if err := os.Rename(f.c.SourceRoot, moved); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(moved, f.c.SourceRoot)
	report, err := ReadStatusAndWriteHealth(t.Context(), config, path, "Cloud backup", 3*time.Hour)
	if err != nil || report.LastSuccess == nil || report.LastAttempt.State != "unchanged" {
		t.Fatal(report, err)
	}
	mirror, err := backuphealth.ReadMirror(path)
	if err != nil || mirror.LastSnapshotAt != report.LastSuccess.SnapshotFinishedAt || mirror.AttemptState != "unchanged" {
		t.Fatal(mirror, err)
	}
	body, _ := os.ReadFile(path)
	for _, secret := range []string{f.c.Remote, f.c.SourceRoot, f.c.RcloneConfig, f.c.StateDir, report.LastSuccess.ID, report.LastSuccess.ArchiveSHA256, "last_verified_at"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("private source or invented verification escaped", secret)
		}
	}
	// A corrupted ledger must not refresh the old healthy projection.
	statePath := filepath.Join(f.c.StateDir, "offsite.json")
	if err := os.WriteFile(statePath, []byte(`{"private":"credentials"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadStatusAndWriteHealth(t.Context(), config, path, "Cloud backup", 3*time.Hour)
	wantCode(t, err, "offsite_state_invalid")
	after, _ := os.ReadFile(path)
	if !bytes.Equal(body, after) {
		t.Fatal("failed source read changed reporter freshness")
	}
}

func TestOffsiteHealthUnsafeOutputCannotReplaceOtherFiles(t *testing.T) {
	f := newFixture(t)
	config := filepath.Join(t.TempDir(), "config.json")
	if err := writeJSON(config, f.c); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "private-data")
	if err := os.WriteFile(victim, []byte("KEEP_PRIVATE"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "health.json")
	if err := os.Symlink(victim, path); err != nil {
		t.Skip(err)
	}
	_, err := ReadStatusAndWriteHealth(t.Context(), config, path, "Cloud", 3*time.Hour)
	wantCode(t, err, "offsite_health_unavailable")
	body, _ := os.ReadFile(victim)
	if string(body) != "KEEP_PRIVATE" {
		t.Fatal("followed output symlink")
	}
	for _, label := range []string{"", strings.Repeat("a", 81), "line\nbreak"} {
		_, err := ReadStatusAndWriteHealth(t.Context(), config, path, label, 3*time.Hour)
		wantCode(t, err, "offsite_configuration_invalid")
	}
}

func TestOffsiteHealthInputCollisionsAndExistingPrivateJSONStayUntouched(t *testing.T) {
	f := newFixture(t)
	config := filepath.Join(t.TempDir(), "offsite-config.json")
	if err := writeJSON(config, f.c); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.c.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(f.c.StateDir, "offsite.json"), Status{Version: 1, Binding: binding(f.c), Owned: []Receipt{}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.c.RcloneConfig, []byte("PRIVATE_CREDENTIALS"), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other-private.json")
	if err := os.WriteFile(other, []byte(`{"keep":"private-settings"}`), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "config-hardlink.json")
	if err := os.Link(config, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{config, alias, f.c.RcloneConfig, filepath.Join(f.c.StateDir, "offsite.json"), other} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ReadStatusAndWriteHealth(t.Context(), config, path, "Cloud", 3*time.Hour)
			wantCode(t, err, "offsite_health_unavailable")
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("private input overwritten", err)
			}
		})
	}
}
