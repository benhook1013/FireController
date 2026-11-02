package offsite

import (
	"context"
	"crypto/sha256"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Activate cancellation only after the real selected stream has read once.
// Like backup's validation-boundary context, this needs no production hook or
// scheduler race. Deadline mode exposes an actual expired standard context only
// at that boundary; earlier validation runs against the live parent context.
type archiveStreamCancellation struct {
	context.Context
	cancel   context.CancelFunc
	terminal context.Context
	phase    string
	reads    atomic.Int64
	fired    atomic.Bool
}

func (c *archiveStreamCancellation) Err() error {
	if c.fired.Load() {
		return c.terminal.Err()
	}
	var pcs [32]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	stream, phase, hash := false, false, false
	for {
		frame, more := frames.Next()
		stream = stream || strings.HasSuffix(frame.Function, "/offsite.(*contextReader).Read")
		phase = phase || strings.HasSuffix(frame.Function, "/offsite."+c.phase)
		hash = hash || strings.HasSuffix(frame.Function, "/offsite.fileHash") || strings.HasSuffix(frame.Function, "/offsite.openHashedArchive")
		if !more {
			break
		}
	}
	if stream && phase && !(c.phase == "unpack" && hash) && c.reads.Add(1) == 2 {
		c.cancel()
		c.fired.Store(true)
		return c.terminal.Err()
	}
	return c.Context.Err()
}

func TestOffsiteArchiveMidStreamCancellationIsNotCorruption(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(filepath.Join(f.dir, "review-captures"), 0700); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 256<<10)
	_, _ = rand.New(rand.NewSource(1)).Read(data)
	if err := os.WriteFile(filepath.Join(f.dir, "review-captures", "review_attempt_"+strings.Repeat("a", 32)+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	bundle := f.bundle(t)
	archive := filepath.Join(t.TempDir(), "valid.tar.gz")
	receipt, err := pack(t.Context(), bundle.Path, archive, f.c)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"pack", "fileHash", "unpack"} {
		for _, deadline := range []bool{false, true} {
			name := phase + "/cancel"
			if deadline {
				name = phase + "/deadline"
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
				ctx := &archiveStreamCancellation{Context: live, cancel: cancel, terminal: terminal, phase: phase}
				if ctx.Err() != nil {
					t.Fatal("fixture was already canceled")
				}
				destination := filepath.Join(t.TempDir(), "partial.tar.gz")
				switch phase {
				case "pack":
					_, err = pack(ctx, bundle.Path, destination, f.c)
				case "fileHash":
					_, _, err = fileHash(ctx, archive, f.c.MaxBundleBytes)
				case "unpack":
					_, err = unpack(ctx, archive, t.TempDir(), receipt, f.c)
				}
				wantCode(t, err, "offsite_cancelled")
				if !ctx.fired.Load() || ctx.reads.Load() < 2 || deadline && ctx.Err() != context.DeadlineExceeded || !deadline && ctx.Err() != context.Canceled {
					t.Fatal("no genuine mid-stream context end", ctx.reads.Load(), ctx.Err())
				}
				if phase == "pack" {
					if _, err := os.Stat(destination); !os.IsNotExist(err) {
						t.Fatal("canceled partial archive survived", err)
					}
				}
				after, err := os.ReadFile(archive)
				if err != nil || sha256.Sum256(after) != sha256.Sum256(before) {
					t.Fatal("cancellation changed valid input archive", err)
				}
			})
		}
	}
	if _, err := unpack(t.Context(), archive, t.TempDir(), receipt, f.c); err != nil {
		t.Fatal("cancellation poisoned later valid recovery", err)
	}
	corrupt := receipt
	corrupt.ArchiveSHA256 = strings.Repeat("0", 64)
	_, err = unpack(t.Context(), archive, t.TempDir(), corrupt, f.c)
	wantCode(t, err, "offsite_archive_invalid")
}

func TestOffsitePendingHashTimeoutRetainsOwnershipAndPending(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	remote := memory()
	if _, err := push(t.Context(), f.c, remote); err != nil {
		t.Fatal(err)
	}
	f.bundle(t)
	remote.uncertainReceipt = true
	_, err := push(t.Context(), f.c, remote)
	wantCode(t, err, "offsite_transport_failed")
	before := state(t, f.c)
	if before.Pending == nil || before.LastSuccess == nil || len(before.Owned) != 1 {
		t.Fatal("fixture lacks pending intent and verified prior copy", before)
	}
	spool := filepath.Join(f.c.StateDir, "pending.tar.gz")
	bytesBefore, err := os.ReadFile(spool)
	if err != nil {
		t.Fatal(err)
	}
	putsBefore := map[string]int{}
	for key, value := range remote.puts {
		putsBefore[key] = value
	}
	config := f.c
	config.Timeout = "1ns"
	_, err = push(t.Context(), config, remote)
	wantCode(t, err, "offsite_cancelled")
	after := state(t, f.c)
	if !reflect.DeepEqual(before.Owned, after.Owned) || !reflect.DeepEqual(before.Pending, after.Pending) || !reflect.DeepEqual(before.LastSuccess, after.LastSuccess) || after.LastAttempt == nil || after.LastAttempt.State != "failed" || after.LastAttempt.Reason != "offsite_cancelled" || !reflect.DeepEqual(remote.puts, putsBefore) || remote.deletes != 0 {
		t.Fatal("timeout changed ownership/pending or falsely claimed corruption", before, after)
	}
	bytesAfter, err := os.ReadFile(spool)
	if err != nil || sha256.Sum256(bytesBefore) != sha256.Sum256(bytesAfter) {
		t.Fatal("timeout changed pending spool", err)
	}
	// The original uncertain publication resumes; its original receipt is reused.
	result, err := push(t.Context(), f.c, remote)
	if err != nil || result.BackupID != before.Pending.ID || state(t, f.c).Pending != nil {
		t.Fatal("timeout prevented supported same-intent recovery", result, err)
	}
	for name, count := range remote.puts {
		if strings.Contains(name, before.Pending.ID) && count != putsBefore[name] {
			t.Fatal("recovery reuploaded committed original archive/receipt", name, count)
		}
	}
}
