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

func TestInstallationInspectCommandReadOnly(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "batch.json")
	provider := filepath.Join(root, "provider.json")
	values := map[string]any{
		path:     offsite.BatchConfig{Version: 1, StateDir: filepath.Join(root, "batch-state"), Configs: []string{provider}, MinFreeBytes: 1},
		provider: offsite.Config{Version: 1, SourceRoot: filepath.Join(root, "source"), InstanceID: "instance_" + strings.Repeat("a", 32), StateDir: filepath.Join(root, "state"), RcloneBinary: filepath.Join(root, "transport"), RcloneConfig: filepath.Join(root, "credentials"), Remote: "remote:backups", Keep: 24, Timeout: "1h", MaxBundleBytes: 1 << 20},
	}
	for p, v := range values {
		raw, _ := json.Marshal(v)
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, diagnostics bytes.Buffer
	if code := run(t.Context(), []string{"backup-installation-inspect", "--config", path}, &out, &diagnostics); code != 0 {
		t.Fatal(code, diagnostics.String())
	}
	var got offsite.BatchInstallationInspection
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.InstallationReady || len(got.Dependencies) != 7 {
		t.Fatal(got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 || diagnostics.Len() != 0 {
		t.Fatal("inspection changed files or produced diagnostics", entries, err, diagnostics.String())
	}
}

func TestInstallationInspectRejectsMutationFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--config", "SECRET", "--health-file", "/tmp/never-create"},
		{"--config", "SECRET", "--config", "OTHER"},
		{"--config", "SECRET", "extra"},
	} {
		var out, diagnostics bytes.Buffer
		code := run(t.Context(), append([]string{"backup-installation-inspect"}, args...), &out, &diagnostics)
		if code != 2 || out.Len() != 0 || strings.Contains(diagnostics.String(), "SECRET") {
			t.Fatal(code, out.String(), diagnostics.String())
		}
	}
}
