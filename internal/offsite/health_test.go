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
