package backup

import (
	"context"
	"github.com/benhook1013/FireController/internal/backupphase"
	"github.com/benhook1013/FireController/internal/core"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedPhaseLeaseBlocksGrowthAndReleases(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	p := filepath.Join(t.TempDir(), "phase.lock")
	if e := os.WriteFile(p, nil, 0600); e != nil {
		t.Fatal(e)
	}
	cfg := m.config
	cfg.PhaseLease = p
	m, err := NewManager(f.store, f.dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m.config.PhaseLease != "" || m.phaseLease != p {
		t.Fatal("runtime lease persisted as restore authority")
	}
	release, e := backupphase.Acquire(t.Context(), p)
	if e != nil {
		t.Fatal(e)
	}
	called := false
	original := m.create
	m.create = func(ctx context.Context, s *core.Store, a, b string) (createdBundle, error) {
		called = true
		return original(ctx, s, a, b)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if e = m.attempt(ctx); e == nil || called {
		t.Fatal("lease permitted growth", e, called)
	}
	release()
	if e = m.attempt(t.Context()); e != nil {
		t.Fatal(e)
	}
	removed := false
	originalRemove := m.remove
	m.remove = func(ctx context.Context, path string, manifest Manifest) error {
		wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if r, e := backupphase.Acquire(wait, p); e == nil {
			r()
			t.Fatal("lease released before retention")
		}
		removed = true
		return originalRemove(ctx, path, manifest)
	}
	if e = m.attempt(t.Context()); e != nil {
		t.Fatal(e)
	}
	if !removed {
		t.Fatal("retention proof did not remove candidate")
	}
	if !called {
		t.Fatal("worker failed to resume")
	}
	r, e := backupphase.Acquire(t.Context(), p)
	if e != nil {
		t.Fatal(e)
	}
	r()
}
