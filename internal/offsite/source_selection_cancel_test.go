package offsite

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
)

// End the real context only after selection has entered Inspect and reached
// database hashing. Earlier directory/manifest selection remains live. This
// matches the backup package's boundary-context tests without a production hook.
type sourceInspectCancellation struct {
	context.Context
	cancel   context.CancelFunc
	terminal context.Context
	fired    atomic.Bool
}

func (c *sourceInspectCancellation) Err() error {
	if c.fired.Load() {
		return c.terminal.Err()
	}
	var pcs [32]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, "/backup.hashPrivate") {
			c.cancel()
			c.fired.Store(true)
			return c.terminal.Err()
		}
		if !more {
			break
		}
	}
	return c.Context.Err()
}

func sourceSelectionBytes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = sha256.Sum256(body)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestOffsiteSourceSelectionAlreadyCancelledIsNotInvalidOrEmpty(t *testing.T) {
	f := newFixture(t)
	bundle := f.bundle(t)
	before := sourceSelectionBytes(t, f.c.SourceRoot)
	entries, err := os.ReadDir(f.c.SourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, rows := range [][]os.DirEntry{nil, entries} {
		got, err := newestEntries(ctx, f.c, rows)
		wantCode(t, err, "offsite_cancelled")
		if got != (backup.Summary{}) {
			t.Fatal("cancellation returned usable backup", got)
		}
	}
	_, err = newest(ctx, f.c)
	wantCode(t, err, "offsite_cancelled")
	remote := memory()
	got, err := push(ctx, f.c, remote)
	wantCode(t, err, "offsite_cancelled")
	if got != (Result{}) || len(remote.files) != 0 || len(remote.puts) != 0 || remote.deletes != 0 {
		t.Fatal("cancelled source selection transferred/pruned", got)
	}
	if _, err := os.Lstat(f.c.StateDir); !os.IsNotExist(err) {
		t.Fatal("already cancelled push created transfer state", err)
	}
	if !reflect.DeepEqual(before, sourceSelectionBytes(t, f.c.SourceRoot)) {
		t.Fatal("cancelled selection changed source")
	}
	if valid, err := newest(t.Context(), f.c); err != nil || valid.ID != bundle.ID {
		t.Fatal("cancellation poisoned later source selection", valid, err)
	}
}

func TestOffsiteSourceSelectionInspectCancellationPreservesVerifiedHistory(t *testing.T) {
	f := newFixture(t)
	first := f.bundle(t)
	remote := memory()
	if _, err := push(t.Context(), f.c, remote); err != nil {
		t.Fatal(err)
	}
	latest := f.bundle(t)
	beforeSource := sourceSelectionBytes(t, f.c.SourceRoot)
	beforeState := sourceSelectionBytes(t, f.c.StateDir)
	beforeLedger := state(t, f.c)
	beforeRemote := map[string][32]byte{}
	for name, body := range remote.files {
		beforeRemote[name] = sha256.Sum256(body)
	}
	beforePuts := map[string]int{}
	for name, count := range remote.puts {
		beforePuts[name] = count
	}
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			live, cancel := context.WithCancel(t.Context())
			defer cancel()
			terminal := live
			if deadline {
				var stop context.CancelFunc
				terminal, stop = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer stop()
			}
			ctx := &sourceInspectCancellation{Context: live, cancel: cancel, terminal: terminal}
			got, err := newest(ctx, f.c)
			wantCode(t, err, "offsite_cancelled")
			if !ctx.fired.Load() || live.Err() != context.Canceled || deadline && ctx.Err() != context.DeadlineExceeded || !deadline && ctx.Err() != context.Canceled || got != (backup.Summary{}) {
				t.Fatal("real Inspect boundary was not interrupted safely", got, ctx.fired.Load(), ctx.Err())
			}
			if !reflect.DeepEqual(beforeSource, sourceSelectionBytes(t, f.c.SourceRoot)) || !reflect.DeepEqual(beforeState, sourceSelectionBytes(t, f.c.StateDir)) || !reflect.DeepEqual(beforeLedger, state(t, f.c)) {
				t.Fatal("interrupted read changed source, pending, ownership or verified success")
			}
		})
	}
	// Selection has no transport side effects; the next normal cycle must publish
	// the same latest source and perform ordinary owned retention, not lose it.
	afterRemote := map[string][32]byte{}
	for name, body := range remote.files {
		afterRemote[name] = sha256.Sum256(body)
	}
	if !reflect.DeepEqual(beforeRemote, afterRemote) || !reflect.DeepEqual(beforePuts, remote.puts) || remote.deletes != 0 || beforeLedger.LastSuccess.ID != first.ID {
		t.Fatal("cancellation uploaded/pruned or lost prior success")
	}
	result, err := push(t.Context(), f.c, remote)
	if err != nil || result.BackupID != latest.ID || state(t, f.c).LastSuccess.ID != latest.ID {
		t.Fatal("normal retry lost source identity", result, err)
	}
}

func TestOffsiteSourceSelectionStillRejectsInvalidManifestAndBundle(t *testing.T) {
	for _, damage := range []string{"manifest", "database"} {
		t.Run(damage, func(t *testing.T) {
			f := newFixture(t)
			bundle := f.bundle(t)
			path := filepath.Join(bundle.Path, "manifest.json")
			if damage == "database" {
				path = filepath.Join(bundle.Path, "firecontroller.sqlite")
			}
			if err := os.WriteFile(path, []byte("invalid private fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			before := sourceSelectionBytes(t, f.c.SourceRoot)
			remote := memory()
			result, err := push(t.Context(), f.c, remote)
			wantCode(t, err, "offsite_source_invalid")
			ledger := state(t, f.c)
			if result != (Result{}) || ledger.LastAttempt == nil || ledger.LastAttempt.State != "failed" || ledger.LastAttempt.Reason != "offsite_source_invalid" || ledger.LastSuccess != nil || ledger.Pending != nil || len(ledger.Owned) != 0 || len(remote.puts) != 0 || remote.deletes != 0 || !reflect.DeepEqual(before, sourceSelectionBytes(t, f.c.SourceRoot)) {
				t.Fatal("invalid source was reclassified or changed transfer state", result, ledger)
			}
		})
	}
}

func TestOffsiteSourceSelectionTimeoutRetainsLedgerAndHealth(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	remote := memory()
	if _, err := push(t.Context(), f.c, remote); err != nil {
		t.Fatal(err)
	}
	f.bundle(t)
	before := state(t, f.c)
	sourceBefore := sourceSelectionBytes(t, f.c.SourceRoot)
	putsBefore := map[string]int{}
	for name, count := range remote.puts {
		putsBefore[name] = count
	}
	deletesBefore := remote.deletes
	// This expires the ordinary operation context before source selection, after
	// the outer initial check. No sleeping, transport hook or deadline relaxation.
	config := f.c
	config.Timeout = "1ns"
	result, err := push(t.Context(), config, remote)
	wantCode(t, err, "offsite_cancelled")
	after := state(t, f.c)
	if result != (Result{}) || after.LastAttempt == nil || after.LastAttempt.State != "failed" || after.LastAttempt.Reason != "offsite_cancelled" || after.LastAttempt.FinishedAt == "" || !reflect.DeepEqual(before.LastSuccess, after.LastSuccess) || !reflect.DeepEqual(before.Owned, after.Owned) || !reflect.DeepEqual(before.Pending, after.Pending) || before.RetentionReason != after.RetentionReason || !reflect.DeepEqual(putsBefore, remote.puts) || remote.deletes != deletesBefore || !reflect.DeepEqual(sourceBefore, sourceSelectionBytes(t, f.c.SourceRoot)) {
		t.Fatal("selection timeout changed verified history, transferred or lost cancellation classification", result, after)
	}
	configPath := emptySourceConfig(t, f.c)
	ordinary, err := ReadStatus(t.Context(), configPath)
	if err != nil || ordinary.LastAttempt.Reason != "offsite_cancelled" || !reflect.DeepEqual(ordinary.LastSuccess, before.LastSuccess) {
		t.Fatal("status lost cancellation or prior verified success", ordinary, err)
	}
	reportDir := t.TempDir()
	if err := os.Chmod(reportDir, 0700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(reportDir, "health.json")
	health, err := ReadStatusAndWriteHealth(t.Context(), configPath, reportPath, "Synthetic", time.Hour)
	if err != nil || !reflect.DeepEqual(health, ordinary) {
		t.Fatalf("health changed raw cancellation/verified-success projection: report=%+v error=%#v", health, err)
	}
}
