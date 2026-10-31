package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/offsite"
)

func TestOffsiteOperatorArgumentsFailBeforeServingOrOpeningSecrets(t *testing.T) {
	for _, args := range [][]string{
		{"backup-push"}, {"backup-push", "--config", "private-secret", "--config", "other"},
		{"backup-fetch", "--config", "private-secret"}, {"backup-fetch", "--config", "private-secret", "--destination", "/new", "--id", "x", "--id", "y"},
		{"backup-offsite-status", "--config", "private-secret", "extra"}, {"backup-push", "--config", "private-secret", "--unsupported", "value"},
	} {
		var out, diagnostics bytes.Buffer
		if code := run(t.Context(), args, &out, &diagnostics); code != 2 || out.Len() != 0 || strings.Contains(diagnostics.String(), "private-secret") {
			t.Fatal(args, code, out.String(), diagnostics.String())
		}
	}
}

func TestOffsiteStatusRequiresNoProviderCredentialOrServingDatabase(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "offsite.json")
	c := offsite.Config{Version: 1, SourceRoot: filepath.Join(dir, "unavailable-source"), StateDir: filepath.Join(dir, "new-state"), InstanceID: "instance_" + strings.Repeat("a", 32), RcloneBinary: filepath.Join(dir, "absent-rclone"), RcloneConfig: filepath.Join(dir, "absent-secret"), Remote: "cloud:backups", Keep: 5, Timeout: "1m", MaxBundleBytes: 1024}
	b, _ := json.Marshal(c)
	if err := os.WriteFile(config, b, 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if code := run(t.Context(), []string{"backup-offsite-status", "--config", config}, &out, &diagnostics); code != 0 {
		t.Fatal(code, diagnostics.String())
	}
	var report offsite.Report
	if json.Unmarshal(out.Bytes(), &report) != nil || report.LastSuccess != nil || report.Retained != 0 || report.Version != 1 {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(c.StateDir); !os.IsNotExist(err) {
		t.Fatal("status created state", err)
	}
}

func TestOffsiteHealthFlagsExportMirrorWithoutChangingRawStatus(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "offsite.json")
	c := offsite.Config{Version: 1, SourceRoot: filepath.Join(dir, "missing-source"), StateDir: filepath.Join(dir, "missing-state"), InstanceID: "instance_" + strings.Repeat("a", 32), RcloneBinary: filepath.Join(dir, "missing-rclone"), RcloneConfig: filepath.Join(dir, "missing-credential"), Remote: "cloud:private-folder", Keep: 1, Timeout: "1m", MaxBundleBytes: 1024}
	body, _ := json.Marshal(c)
	if err := os.WriteFile(config, body, 0600); err != nil {
		t.Fatal(err)
	}
	var original, reported, diagnostics bytes.Buffer
	if code := run(t.Context(), []string{"backup-offsite-status", "--config", config}, &original, &diagnostics); code != 0 {
		t.Fatal(code, diagnostics.String())
	}
	path := filepath.Join(dir, "health.json")
	if code := run(t.Context(), []string{"backup-offsite-status", "--config", config, "--health-file", path, "--health-label", "Cloud copy", "--health-max-age", "3h"}, &reported, &diagnostics); code != 0 {
		t.Fatal(code, diagnostics.String())
	}
	if !bytes.Equal(original.Bytes(), reported.Bytes()) {
		t.Fatal("status stdout changed when publishing redacted mirror")
	}
	mirror, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(mirror, []byte(`"label":"Cloud copy"`)) || bytes.Contains(mirror, []byte(c.Remote)) || bytes.Contains(mirror, []byte(c.RcloneConfig)) {
		t.Fatal("missing or private mirror", err)
	}
	if _, err := os.Stat(c.StateDir); !os.IsNotExist(err) {
		t.Fatal("status initialized transfer state", err)
	}
}

func TestOffsiteHealthInvalidFlagsFailBeforeConfigurationRead(t *testing.T) {
	for _, extra := range [][]string{{"--health-label", "Cloud"}, {"--health-file", "relative"}, {"--health-file", "/private/health", "--health-label", "line\nbreak"}, {"--health-file", "/private/health", "--health-max-age", "0s"}, {"--health-file", "/private/health", "--health-max-age", "1.5s"}, {"--health-file", "/private/health", "--health-file", "/other"}} {
		var out, diagnostics bytes.Buffer
		args := append([]string{"backup-offsite-status", "--config", "DO_NOT_READ_PRIVATE"}, extra...)
		if code := run(t.Context(), args, &out, &diagnostics); code != 2 || out.Len() != 0 || strings.Contains(diagnostics.String(), "DO_NOT_READ_PRIVATE") {
			t.Fatal(code, out.String(), diagnostics.String())
		}
	}
}
