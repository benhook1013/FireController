package offsite

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOffsiteFetchRefusesTransferStateDestinationsBeforeSession(t *testing.T) {
	for _, mode := range []string{"state", "child", "work-child", "destination-alias", "configured-state-alias", "absent-state"} {
		t.Run(mode, func(t *testing.T) {
			if strings.Contains(mode, "alias") && runtime.GOOS == "windows" {
				t.Skip("POSIX symlink fixture")
			}
			f := newFixture(t)
			stateRoot := f.c.StateDir
			work := filepath.Join(stateRoot, "work")
			sentinel := filepath.Join(work, "keep")
			if mode != "absent-state" {
				if err := os.MkdirAll(work, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(work, ".owner"), []byte(binding(f.c)), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(sentinel, []byte("preserve existing transfer work"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			destination := filepath.Join(stateRoot, "work", "recovered")
			switch mode {
			case "state":
				destination = stateRoot
			case "child", "absent-state":
				destination = filepath.Join(stateRoot, "recovered")
			case "destination-alias", "configured-state-alias":
				alias := filepath.Join(t.TempDir(), "state-alias")
				if err := os.Symlink(stateRoot, alias); err != nil {
					t.Fatal(err)
				}
				if mode == "destination-alias" {
					destination = filepath.Join(alias, "work", "recovered")
				} else {
					f.c.StateDir = alias
				}
			}
			config := filepath.Join(t.TempDir(), "offsite.json")
			if err := writeJSON(config, f.c); err != nil {
				t.Fatal(err)
			}
			// Provider configuration/executable deliberately do not exist. A
			// refusal must precede even session construction and credential reads.
			_, err := Fetch(t.Context(), config, "", destination)
			wantCode(t, err, "offsite_configuration_invalid")
			if mode == "absent-state" {
				if _, err := os.Lstat(stateRoot); !os.IsNotExist(err) {
					t.Fatal("refusal initialized transfer state", err)
				}
			} else if body, err := os.ReadFile(sentinel); err != nil || string(body) != "preserve existing transfer work" {
				t.Fatal("refusal reclaimed existing session work", err)
			}
			if _, err := os.Lstat(filepath.Join(stateRoot, "transfer.lock")); !os.IsNotExist(err) {
				t.Fatal("refusal opened a transfer session", err)
			}
		})
	}
}

func TestOffsiteFetchAllowsOrdinaryDestinationsOutsideTransferState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX transport fixture")
	}
	for _, mode := range []string{"separate", "source-root", "state-sibling"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			if err := os.WriteFile(f.c.RcloneBinary, []byte("#!/bin/sh\nexit 3\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.c.RcloneConfig, []byte("private synthetic fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(t.TempDir(), "offsite.json")
			if err := writeJSON(config, f.c); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "recovered")
			if mode == "source-root" {
				destination = filepath.Join(f.c.SourceRoot, "recovered")
			} else if mode == "state-sibling" {
				destination = f.c.StateDir + "-recovered"
			}
			_, err := Fetch(t.Context(), config, "", destination)
			wantCode(t, err, "offsite_backup_not_found")
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("missing remote backup created a destination", err)
			}
		})
	}
}
