package offsite

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOffsiteFetchMissingBackupReportsOperatorError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX transport fixture")
	}
	for _, id := range []string{"", "backup_" + strings.Repeat("a", 32)} {
		t.Run(id, func(t *testing.T) {
			f := newFixture(t)
			if err := os.WriteFile(f.c.RcloneBinary, []byte("#!/bin/sh\nexit 3\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.c.RcloneConfig, []byte("synthetic private configuration"), 0600); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(t.TempDir(), "offsite.json")
			if err := writeJSON(config, f.c); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "recovered")
			_, err := Fetch(t.Context(), config, id, destination)
			wantCode(t, err, "offsite_backup_not_found")
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("missing backup created a recovery directory", err)
			}
		})
	}
}

type missingVerificationTransport struct{ *memoryTransport }

func (m missingVerificationTransport) get(context.Context, string, string) error { return notFound }

func TestOffsiteMissingRemoteArchivePersistsSpecificFailure(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	_, err := push(t.Context(), f.c, missingVerificationTransport{memory()})
	wantCode(t, err, "offsite_backup_not_found")
	s := state(t, f.c)
	if s.LastAttempt == nil || s.LastAttempt.State != "failed" || s.LastAttempt.Reason != "offsite_backup_not_found" || s.LastSuccess != nil {
		t.Fatal("failure lost operator meaning", s)
	}
}
