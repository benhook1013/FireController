package offsite

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBatchVerificationExactReceiptAndRestart(t *testing.T) {
	f := newFixture(t)
	summary := f.bundle(t)
	p := &batchPreparation{work: t.TempDir()}
	archive := filepath.Join(t.TempDir(), "archive")
	r, err := p.linkArchive(t.Context(), summary, archive, f.c)
	if err != nil {
		t.Fatal(err)
	}
	if !p.verified[r] {
		t.Fatal("preparation missing native validation")
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	tr := memory()
	s := &session{c: f.c, work: t.TempDir(), transport: tr, batch: p}
	tr.files[s.object(r, "archive.tar.gz")] = data
	if err = s.verifyRemote(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	tr.corruptDownload = true
	if err = s.verifyRemote(t.Context(), r); err == nil {
		t.Fatal("cached validation skipped remote hash")
	}
	tr.corruptDownload = false
	changed := r
	changed.EventSequence++
	if err = s.verifyRemote(t.Context(), changed); err == nil {
		t.Fatal("changed receipt reused proof")
	}
	// Fresh process-local state must establish its own native validation.
	fresh := &batchPreparation{}
	s.batch = fresh
	if len(fresh.verified) != 0 {
		t.Fatal("persisted cache")
	}
	if err = s.verifyRemote(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if !fresh.verified[r] {
		t.Fatal("restart did not validate")
	}
}

func TestBatchMeasuredExpansionBelowConfiguredMaximum(t *testing.T) {
	f := newFixture(t)
	summary := f.bundle(t)
	f.c.MaxBundleBytes = 2 << 30
	p := &batchPreparation{work: t.TempDir()}
	archive := filepath.Join(t.TempDir(), "archive")
	r, err := p.linkArchive(t.Context(), summary, archive, f.c)
	if err != nil {
		t.Fatal(err)
	}
	need, err := archiveExpansionBudget(t.Context(), archive, r, f.c)
	if err != nil {
		t.Fatal(err)
	}
	if need <= 0 || need >= f.c.MaxBundleBytes {
		t.Fatalf("not measured: %d", need)
	}
	t.Logf("archive=%d expansion allocation budget=%d configured bound=%d", r.ArchiveBytes, need, f.c.MaxBundleBytes)
}
