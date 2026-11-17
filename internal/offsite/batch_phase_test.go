package offsite

import (
	"context"
	"github.com/benhook1013/FireController/internal/backupphase"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBatchPhaseLeaseBlocksStagingAndResumes(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	b.PhaseLease = filepath.Join(t.TempDir(), "phase.lock")
	if e := os.WriteFile(b.PhaseLease, nil, 0600); e != nil {
		t.Fatal(e)
	}
	release, e := backupphase.Acquire(t.Context(), b.PhaseLease)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, e = pushBatch(ctx, b, cs, ts); e == nil {
		t.Fatal("batch overlapped")
	}
	if _, e = os.Stat(filepath.Join(b.StateDir, "work")); !os.IsNotExist(e) {
		t.Fatal("created staging before lease", e)
	}
	release()
	if _, e = pushBatch(t.Context(), b, cs, ts); e != nil {
		t.Fatal(e)
	}
	r, e := backupphase.Acquire(t.Context(), b.PhaseLease)
	if e != nil {
		t.Fatal(e)
	}
	r()
}
func TestBatchPhaseLeaseSurvivesSourceLoss(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	b.PhaseLease = filepath.Join(t.TempDir(), "phase.lock")
	if e := os.WriteFile(b.PhaseLease, nil, 0600); e != nil {
		t.Fatal(e)
	}
	for _, tr := range ts {
		tr.(*memoryTransport).corruptDownload = true
	}
	if _, e := pushBatch(t.Context(), b, cs, ts); e == nil {
		t.Fatal("expected pending")
	}
	if e := os.RemoveAll(cs[0].SourceRoot); e != nil {
		t.Fatal(e)
	}
	for _, tr := range ts {
		tr.(*memoryTransport).corruptDownload = false
	}
	got, e := pushBatch(t.Context(), b, cs, ts)
	if e != nil || len(got.Destinations) != 3 {
		t.Fatal(got, e)
	}
}

func TestBatchPhaseLeasePreservesBusyRefusal(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	b.PhaseLease = filepath.Join(t.TempDir(), "phase.lock")
	if e := os.WriteFile(b.PhaseLease, nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(b.StateDir, 0700); e != nil {
		t.Fatal(e)
	}
	f, e := os.OpenFile(filepath.Join(b.StateDir, "batch.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if e = lock(f); e != nil {
		t.Fatal(e)
	}
	release, e := backupphase.Acquire(t.Context(), b.PhaseLease)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, e = pushBatch(ctx, b, cs, ts)
	if e == nil || ctx.Err() != nil {
		t.Fatal("duplicate waited for shared phase", e, ctx.Err())
	}
}
