package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func backupCommandSource(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Private managed-bundle filesystem proof requires Unix permissions")
	}
	dir := filepath.Join(t.TempDir(), "source")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, ensure := range []func(context.Context, *core.Store) error{jobs.EnsureSchema, inbox.EnsureSchema, reviews.EnsureSchema, reviewexec.EnsureSchema, reviewexec.EnsureEvidenceSchema, reviewexec.EnsureAdjudicationSchema, reviewexec.EnsurePolicySchema} {
		if err = ensure(t.Context(), store); err != nil {
			t.Fatal(err)
		}
	}
	return dir, store.InstanceID()
}

func runBackupResult(t *testing.T, args ...string) []byte {
	t.Helper()
	var out, diagnostics bytes.Buffer
	if code := run(t.Context(), args, &out, &diagnostics); code != 0 || diagnostics.Len() != 0 || out.Len() > 4096 || bytes.Count(out.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("operator command failed: code=%d diagnostics=%s", code, diagnostics.String())
	}
	return out.Bytes()
}

func TestBackupOperatorCommandsRealBundleAndInactiveStartup(t *testing.T) {
	source, instance := backupCommandSource(t)
	root := filepath.Join(t.TempDir(), "bundles")
	var created, inspected backup.Summary
	if err := json.Unmarshal(runBackupResult(t, "backup", "--data-dir", source, "--destination", root), &created); err != nil || created.State != "complete" || created.SourceInstanceID != instance || created.Path == "" {
		t.Fatal("invalid create summary", created, err)
	}
	if err := json.Unmarshal(runBackupResult(t, "backup-inspect", "--bundle", created.Path, "--timeout=1m"), &inspected); err != nil || inspected.State != "verified" || inspected.ID != created.ID || inspected.SourceInstanceID != instance {
		t.Fatal("inspect changed bundle identity", inspected, err)
	}
	restoredPath := filepath.Join(t.TempDir(), "new-inactive")
	var restored backup.RestoreSummary
	if err := json.Unmarshal(runBackupResult(t, "restore", "--bundle", created.Path, "--data-dir", restoredPath), &restored); err != nil || restored.State != "inactive" || restored.BundleID != created.ID || restored.SourceInstanceID != instance || restored.InstanceID == instance || restored.InstanceID == "" {
		t.Fatal("restore fabricated active/original identity", restored, err)
	}
	store, err := core.Open(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if store.InstanceID() != instance {
		t.Fatal("backup changed source instance")
	}
	_ = store.Close()
	var out, diagnostics bytes.Buffer
	code := run(t.Context(), []string{"--data-dir", restoredPath, "--listen", "127.0.0.1:0", "--review-catalog", filepath.Join(t.TempDir(), "missing-private-catalog"), "--github-token-file", filepath.Join(t.TempDir(), "missing-private-token")}, &out, &diagnostics)
	if code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"restore_inactive"`) || strings.Contains(diagnostics.String(), restoredPath) {
		t.Fatal("inactive guard did not precede catalog/listener", code, out.String(), diagnostics.String())
	}
	if _, err = os.Lstat(filepath.Join(restoredPath, "bootstrap-admin.token")); !os.IsNotExist(err) {
		t.Fatal("inactive startup created bootstrap credential", err)
	}
	// Restore collision cannot replace an existing destination or marker.
	sentinel := filepath.Join(restoredPath, "must-survive")
	if err = os.WriteFile(sentinel, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostics.Reset()
	code = run(t.Context(), []string{"restore", "--bundle", created.Path, "--data-dir", restoredPath}, &out, &diagnostics)
	data, err := os.ReadFile(sentinel)
	if code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"backup_exists"`) || err != nil || string(data) != "retained" {
		t.Fatal("restore collision replaced state", code, diagnostics.String(), err)
	}
}

func TestBackupOperatorRejectsInvalidArgumentsBeforeFilesystem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-stay-absent")
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	base := []string{"backup", "--data-dir", source, "--destination", destination}
	cases := [][]string{{"backup"}, {"backup-inspect"}, {"restore", "--bundle", source}, {"backup", "--data-dir"}, {"backup-inspect", "--bundle", source, "position"}, {"restore", "--bundle", source, "--data-dir", destination, "--destination", destination}}
	for _, suffix := range [][]string{{"--data-dir", source}, {"-data-dir=" + source}, {"--unknown", "PRIVATE_UNEXPOSED_VALUE"}, {"--timeout", "0"}, {"--timeout", "-1s"}, {"--timeout", "24h1ns"}, {"--timeout", "PRIVATE_UNEXPOSED_VALUE"}, {"--timeout", "1s", "--timeout=2s"}, {"--listen", "127.0.0.1:0"}} {
		cases = append(cases, append(append([]string{}, base...), suffix...))
	}
	for _, args := range cases {
		var out, diagnostics bytes.Buffer
		code := run(t.Context(), args, &out, &diagnostics)
		if code != 2 || out.Len() != 0 || !json.Valid(diagnostics.Bytes()) || strings.Contains(diagnostics.String(), root) || strings.Contains(diagnostics.String(), "PRIVATE_") {
			t.Fatal("invalid command reflected inputs or continued", args, code, diagnostics.String())
		}
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("invalid operator options touched filesystem", err)
	}
}

func TestBackupOperatorMissingEmptyAndForeignSourceCannotBootstrap(t *testing.T) {
	for _, kind := range []string{"missing-directory", "missing-db", "empty-db", "foreign-db"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			dbPath := filepath.Join(source, "firecontroller.sqlite")
			if kind != "missing-directory" {
				if err := os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "empty-db" || kind == "foreign-db" {
				if err := os.WriteFile(dbPath, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "foreign-db" {
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`CREATE TABLE unrelated(value TEXT); INSERT INTO unrelated VALUES('keep me')`)
				_ = db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(dbPath)
			var out, diagnostics bytes.Buffer
			destination := filepath.Join(root, "bundles")
			code := run(t.Context(), []string{"backup", "--data-dir", source, "--destination", destination}, &out, &diagnostics)
			if code != 3 || out.Len() != 0 || !json.Valid(diagnostics.Bytes()) || strings.Contains(diagnostics.String(), source) {
				t.Fatal("unsafe source accepted/reflected", code, diagnostics.String())
			}
			if _, err := os.Lstat(filepath.Join(source, "bootstrap-admin.token")); !os.IsNotExist(err) {
				t.Fatal("backup bootstrapped source", err)
			}
			if after, _ := os.ReadFile(dbPath); !bytes.Equal(before, after) {
				t.Fatal("preflight modified invalid source")
			}
			if kind == "missing-directory" {
				if _, err := os.Lstat(source); !os.IsNotExist(err) {
					t.Fatal("missing source directory created", err)
				}
			}
			if kind == "missing-db" {
				if _, err := os.Lstat(dbPath); !os.IsNotExist(err) {
					t.Fatal("missing source database created", err)
				}
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("invalid source created destination", err)
			}
		})
	}
}

func TestBackupOperatorCancellationAndIncompleteRestoreFence(t *testing.T) {
	source, _ := backupCommandSource(t)
	destination := filepath.Join(t.TempDir(), "absent")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, diagnostics bytes.Buffer
	if code := run(ctx, []string{"backup", "--data-dir", source, "--destination", destination}, &out, &diagnostics); code != 3 || out.Len() != 0 {
		t.Fatal("canceled operator command continued", code, diagnostics.String())
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("canceled command published state", err)
	}
	out.Reset()
	diagnostics.Reset()
	if code := run(t.Context(), []string{"backup", "--data-dir", source, "--destination", destination, "--timeout", "1ns"}, &out, &diagnostics); code != 3 || out.Len() != 0 {
		t.Fatal("explicit finite deadline was not enforced", code, diagnostics.String())
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("expired operation published state", err)
	}
	for _, kind := range []string{"regular", "dangling-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, ".restore-in-progress")
			if kind == "regular" {
				if err := os.WriteFile(marker, []byte("incomplete"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(filepath.Join(dir, "missing"), marker); err != nil {
				t.Skip("symlink unavailable")
			}
			var out, diagnostics bytes.Buffer
			code := run(t.Context(), []string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--review-catalog", filepath.Join(dir, "missing")}, &out, &diagnostics)
			if code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"restore_inactive"`) {
				t.Fatal("partial restore entered daemon setup", code, diagnostics.String())
			}
			if _, err := os.Lstat(filepath.Join(dir, "firecontroller.sqlite")); !os.IsNotExist(err) {
				t.Fatal("partial restore bootstrapped database", err)
			}
		})
	}
}
